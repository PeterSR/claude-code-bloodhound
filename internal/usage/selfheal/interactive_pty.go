package selfheal

// This file is deliberately kept free of bloodhound-specific concepts
// (extractors, the bridge, "required fields", etc.) so that the
// "drive interactive claude over a pty" piece can be lifted into its
// own Go module later without dragging the heal-specific glue along.
// Anything in here should be applicable to the generic problem of
// "run interactive claude, hand it a prompt, observe."

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
	"github.com/creack/pty"
)

// ClaudeLaunch is the set of knobs we hand to interactive `claude` when
// driving it from Go. Empty strings / zero values mean "don't pass that
// flag" rather than "pass an empty value."
type ClaudeLaunch struct {
	// Binary is the resolved path to `claude`. Empty defaults to "claude".
	Binary string

	// MCPConfig is the path passed to --mcp-config. Empty = no flag.
	MCPConfig string

	// StrictMCPConfig adds --strict-mcp-config so the launched session
	// loads only the servers in MCPConfig (not the user's global ones).
	StrictMCPConfig bool

	// AllowedTools is the comma-joined list for --allowedTools. Auto-
	// approves listed tools without prompting in the TUI.
	AllowedTools string

	// AppendSystemPrompt is forwarded to --append-system-prompt.
	AppendSystemPrompt string

	// PermissionMode is forwarded to --permission-mode (default,
	// acceptEdits, bypassPermissions, plan). Empty = no flag.
	PermissionMode string

	// SessionID, if non-empty, is forwarded to --session-id. Lets the
	// caller correlate the run with the JSONL file claude persists.
	SessionID string

	// ExtraArgs are appended verbatim to the command line. The prompt
	// is *not* part of this — callers send it via SendPrompt after the
	// input row is ready.
	ExtraArgs []string

	// Cwd, if non-empty, becomes the child's working directory.
	Cwd string
}

// ClaudeSession is one running interactive claude under Go control.
type ClaudeSession struct {
	cmd  *exec.Cmd
	pty  *os.File
	sess *Session

	// exited closes when the child process has been reaped. Used by
	// WaitForDone to notice that claude exited on its own without
	// having to poll cmd.ProcessState (which only populates after
	// Wait() returns).
	exited chan struct{}
	// waitErr is the result of cmd.Wait(); written before close(exited).
	waitErr error
}

// LaunchClaude spawns interactive claude with the configured flags and
// a subscription-only env (strips ANTHROPIC_* provider keys, sets a
// TUI-friendly TERM). Caller owns the returned session and must Close().
func LaunchClaude(ctx context.Context, l ClaudeLaunch) (*ClaudeSession, error) {
	bin := l.Binary
	if bin == "" {
		bin = "claude"
	}
	args := buildClaudeArgs(l)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = subscriptionEnv()
	if l.Cwd != "" {
		cmd.Dir = l.Cwd
	}

	master, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("spawn claude pty: %w", err)
	}
	cs := &ClaudeSession{
		cmd:    cmd,
		pty:    master,
		sess:   NewSession(master, nil),
		exited: make(chan struct{}),
	}
	go func() {
		cs.waitErr = cs.cmd.Wait()
		close(cs.exited)
	}()
	return cs, nil
}

func buildClaudeArgs(l ClaudeLaunch) []string {
	var args []string
	if l.MCPConfig != "" {
		args = append(args, "--mcp-config", l.MCPConfig)
	}
	if l.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	if l.AllowedTools != "" {
		args = append(args, "--allowedTools", l.AllowedTools)
	}
	if l.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", l.AppendSystemPrompt)
	}
	if l.PermissionMode != "" {
		args = append(args, "--permission-mode", l.PermissionMode)
	}
	if l.SessionID != "" {
		args = append(args, "--session-id", l.SessionID)
	}
	args = append(args, l.ExtraArgs...)
	return args
}

// subscriptionEnv strips provider-API env vars so the spawned claude
// definitively uses the subscription login flow (mirrors claude-p's
// SUBSCRIPTION_BACKEND_ENV_OVERRIDES handling). Also sets NO_COLOR for
// quieter rendering and TERM so the TUI is willing to draw.
func subscriptionEnv() []string {
	strip := map[string]struct{}{
		"ANTHROPIC_API_KEY":   {},
		"ANTHROPIC_AUTH_TOKEN": {},
		"ANTHROPIC_BASE_URL":  {},
	}
	src := os.Environ()
	out := make([]string, 0, len(src)+2)
	hasTerm := false
	for _, kv := range src {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		k := kv[:eq]
		if _, drop := strip[k]; drop {
			continue
		}
		if k == "TERM" {
			hasTerm = true
		}
		out = append(out, kv)
	}
	if !hasTerm {
		out = append(out, "TERM=xterm-256color")
	}
	out = append(out, "NO_COLOR=1")
	return out
}

// ErrProcessExited is returned by WaitForDone if claude exits before the
// done signal fires.
var ErrProcessExited = errors.New("claude process exited")

// ErrTimeout is returned by WaitForDone if the budget elapses without
// the done signal firing.
var ErrTimeout = errors.New("interactive orchestrator timeout")

// WaitForReady waits up to budget for the input prompt to appear,
// handling the "Is this a project you trust?" modal claude shows the
// first time a folder is opened. Returns nil once the main input row
// is visible.
func (cs *ClaudeSession) WaitForReady(ctx context.Context, budget time.Duration) error {
	const settle = 400 * time.Millisecond
	deadline := time.Now().Add(budget)
	trustHandled := false

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		raw, sinceLast := cs.sess.snapshot()
		if len(raw) == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		screen := usage.RenderVT(raw)
		lower := strings.ToLower(screen)

		if !trustHandled {
			if strings.Contains(lower, "trust this folder") || strings.Contains(lower, "do you trust") {
				if sinceLast >= settle {
					// Option 1 == Yes; \r confirms.
					if _, err := cs.pty.Write([]byte("\r")); err != nil {
						return fmt.Errorf("answer trust modal: %w", err)
					}
					trustHandled = true
					time.Sleep(750 * time.Millisecond)
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			trustHandled = true
		}

		if usage.HasInputPrompt(screen) && sinceLast >= settle {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ErrTimeout
}

// SendPrompt types the prompt + Enter into the live pty. Splits the
// text from the Enter so claude has a moment to ingest before submit
// (paste-detection heuristics inside claude have, historically, been
// twitchy about huge single writes).
func (cs *ClaudeSession) SendPrompt(prompt string) error {
	if _, err := cs.pty.Write([]byte(prompt)); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := cs.pty.Write([]byte("\r")); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	return nil
}

// WaitForDone blocks until one of: done closes (nil return), the
// process exits (ErrProcessExited), ctx ends (ctx.Err()), or budget
// elapses (ErrTimeout). If budget is 0, the only timeout comes from ctx.
func (cs *ClaudeSession) WaitForDone(ctx context.Context, done <-chan struct{}, budget time.Duration) error {
	var deadline <-chan time.Time
	if budget > 0 {
		t := time.NewTimer(budget)
		defer t.Stop()
		deadline = t.C
	}

	// Tickle the process-exit signal cheaply via polling cmd.ProcessState.
	// exec.Cmd has no "exited" channel and we don't want to call Wait()
	// here (it races with our own pty drain goroutine and the deferred
	// kill in the caller). 200ms is short enough to feel responsive and
	// long enough not to burn CPU on the watchdog.
	exitTick := time.NewTicker(200 * time.Millisecond)
	defer exitTick.Stop()

	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return ErrTimeout
		case <-exitTick.C:
			if cs.cmd.ProcessState != nil && cs.cmd.ProcessState.Exited() {
				return ErrProcessExited
			}
		}
	}
}

// Snapshot returns the rendered screen at the current instant.
func (cs *ClaudeSession) Snapshot() string {
	return cs.sess.renderGrid()
}

// Exit asks claude to exit cleanly, falling back to SIGKILL after a
// short grace. Safe to call multiple times.
func (cs *ClaudeSession) Exit() {
	// /exit isn't a real slash command in every claude version, but
	// double-Ctrl-C does the right thing universally.
	_, _ = cs.pty.Write([]byte{0x03})
	time.Sleep(150 * time.Millisecond)
	_, _ = cs.pty.Write([]byte{0x03})

	// Give it ~750ms to teardown gracefully before we kill it.
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cs.cmd.ProcessState != nil && cs.cmd.ProcessState.Exited() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cs.cmd.Process != nil {
		_ = cs.cmd.Process.Kill()
	}
}

// Close releases the pty and ensures the process is reaped. After
// Close, the ClaudeSession is unusable.
func (cs *ClaudeSession) Close() {
	cs.sess.Close()
	_ = cs.pty.Close()
	if cs.cmd.Process != nil {
		_ = cs.cmd.Process.Kill()
		_, _ = cs.cmd.Process.Wait()
	}
}

// ClassifyInteractiveFailure returns a short reason string for common
// failure surfaces visible in the rendered TUI, or "" if nothing
// recognisable is present. Patterns mirror those used by claude-p.
func ClassifyInteractiveFailure(screen string) string {
	low := strings.ToLower(screen)
	switch {
	case strings.Contains(low, "failed to authenticate"),
		strings.Contains(low, "api error: 403"),
		strings.Contains(low, "please run /login"):
		return "auth_blocked"
	case strings.Contains(low, "hit your limit"),
		strings.Contains(low, "approaching usage limit"),
		strings.Contains(low, "5-hour limit"):
		return "rate_limit"
	case strings.Contains(low, "do you trust") && strings.Contains(low, "folder"):
		return "workspace_trust_blocked"
	case strings.Contains(low, "permission") && (strings.Contains(low, "allow") || strings.Contains(low, "deny")):
		return "tool_approval_blocked"
	}
	return ""
}

// NewSessionID returns a 16-byte hex token suitable for --session-id.
// Not strictly a UUID but claude accepts any unique identifier here.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Time-based fallback is fine — uniqueness, not unguessability,
		// is what claude cares about here.
		return fmt.Sprintf("bh-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
