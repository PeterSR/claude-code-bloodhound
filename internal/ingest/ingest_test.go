package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// TestLooksLikeUUID covers the guard that stops a garbled subagent parent
// directory from being trusted as a session link (see its use in Run).
func TestLooksLikeUUID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical lowercase", "a850d051-2b0d-455b-991c-a0a434be269f", true},
		{"canonical uppercase", "A850D051-2B0D-455B-991C-A0A434BE269F", true},
		{"missing dashes", "a850d0512b0d455b991ca0a434be269f", false},
		{"subagent filename stem, not a session dir", "agent-a6e5d652439ec9e75", false},
		{"too short", "a850d051-2b0d-455b-991c", false},
		{"empty", "", false},
		{"project dir name", "-home-peter-dev-personal-cad-web", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeUUID(c.in); got != c.want {
				t.Errorf("looksLikeUUID(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestFindSessionFiles_DiscoversSubagentsWithParentUUID covers the second
// glob pattern added alongside the original <project>/<session>.jsonl one:
// a subagent transcript one level deeper must be found, marked isSubagent,
// and carry the parent UUID read off its own containing directory (two
// levels up from the file, one up from "subagents"), all without
// disturbing what the original pattern matches.
func TestFindSessionFiles_DiscoversSubagentsWithParentUUID(t *testing.T) {
	root := t.TempDir()
	const parentUUID = "a850d051-2b0d-455b-991c-a0a434be269f"

	topPath := filepath.Join(root, "proj1", "session-top.jsonl")
	subPath := filepath.Join(root, "proj1", parentUUID, "subagents", "agent-a6e5d652439ec9e75.jsonl")
	mustWriteFile(t, topPath, "{}\n")
	mustWriteFile(t, subPath, "{}\n")

	files, err := findSessionFiles(root)
	if err != nil {
		t.Fatalf("findSessionFiles: %v", err)
	}

	var gotTop, gotSub *sessionFile
	for i := range files {
		switch files[i].path {
		case topPath:
			gotTop = &files[i]
		case subPath:
			gotSub = &files[i]
		}
	}

	if gotTop == nil {
		t.Fatalf("top-level file %s not discovered", topPath)
	}
	if gotTop.isSubagent {
		t.Errorf("top-level file wrongly marked isSubagent")
	}

	if gotSub == nil {
		t.Fatalf("subagent file %s not discovered", subPath)
	}
	if !gotSub.isSubagent {
		t.Errorf("subagent file not marked isSubagent")
	}
	if gotSub.parentUUID != parentUUID {
		t.Errorf("parentUUID = %q, want %q", gotSub.parentUUID, parentUUID)
	}
	if gotSub.project != "proj1" {
		t.Errorf("subagent project = %q, want %q", gotSub.project, "proj1")
	}
}

// TestFindSessionFiles_DiscoversWorkflowAgentsWithParentUUID covers the
// third glob, one level deeper than a plain subagent: a Workflow-tool
// agent under <parent-uuid>/subagents/workflows/<wf-id>/agent-*.jsonl must
// be found, marked isSubagent (reusing the same flag a Task-tool subagent
// uses, see the field doc on sessionFile), and carry the correct parentUUID
// and project despite sitting two directories deeper than the plain
// subagent case: exactly the depth mismatch that broke a fixed-depth
// assumption before this shape existed (see
// TestParseFile_TrustsSubagentContextProjectAtAnyDepth in jsonl_test.go for
// the parseFile side of that fix). journal.jsonl, living in the same
// <wf-id> directory, must not be discovered at all.
func TestFindSessionFiles_DiscoversWorkflowAgentsWithParentUUID(t *testing.T) {
	root := t.TempDir()
	const parentUUID = "a850d051-2b0d-455b-991c-a0a434be269f"

	wfAgentPath := filepath.Join(root, "proj1", parentUUID, "subagents", "workflows", "wf_abc123", "agent-a026760a272779ef1.jsonl")
	journalPath := filepath.Join(root, "proj1", parentUUID, "subagents", "workflows", "wf_abc123", "journal.jsonl")
	mustWriteFile(t, wfAgentPath, "{}\n")
	mustWriteFile(t, journalPath, `{"type":"started","agentId":"a026760a272779ef1"}`+"\n")

	files, err := findSessionFiles(root)
	if err != nil {
		t.Fatalf("findSessionFiles: %v", err)
	}

	var gotWF *sessionFile
	for i := range files {
		switch files[i].path {
		case wfAgentPath:
			gotWF = &files[i]
		case journalPath:
			t.Fatalf("journal.jsonl must not be discovered as a transcript: %s", journalPath)
		}
	}

	if gotWF == nil {
		t.Fatalf("workflow agent file %s not discovered", wfAgentPath)
	}
	if !gotWF.isSubagent {
		t.Errorf("workflow agent not marked isSubagent (it should reuse the flag, not get a separate one)")
	}
	if gotWF.parentUUID != parentUUID {
		t.Errorf("parentUUID = %q, want %q", gotWF.parentUUID, parentUUID)
	}
	if gotWF.project != "proj1" {
		t.Errorf("project = %q, want %q", gotWF.project, "proj1")
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// subagentJSONLRecord returns one assistant record with a cwd field set, the
// same shape parseFile expects (built on assistantRecord/usageMap from
// jsonl_test.go so both files agree on what a minimal valid record looks
// like).
func subagentJSONLRecord(ts, requestID, messageID, model, cwd string, usage map[string]any) map[string]any {
	rec := assistantRecord(ts, requestID, messageID, model, usage)
	rec["cwd"] = cwd
	return rec
}

// TestRun_SubagentDiscoveryEndToEnd drives the real Run() pipeline against a
// throwaway store and a fabricated ~/.claude/projects layout, covering the
// two behaviors the spec calls out explicitly: a subagent under a valid
// parent UUID is ingested with parent_session_uuid/cwd/project set correctly
// (rather than colliding with the parent's own turn_idx sequence: see the
// comment on session_uuid in parseFile for why sessionId can't be trusted
// here), and a subagent under a directory that doesn't look like a session
// UUID is skipped with a logged reason instead of writing a fabricated link.
func TestRun_SubagentDiscoveryEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_STATE_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", dataDir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.DB.Close()

	projectsDir := t.TempDir()
	const (
		parentUUID  = "a850d051-2b0d-455b-991c-a0a434be269f"
		badParentID = "not-a-uuid"
		project     = "proj1"
	)

	topPath := filepath.Join(projectsDir, project, parentUUID+".jsonl")
	goodSubPath := filepath.Join(projectsDir, project, parentUUID, "subagents", "agent-good.jsonl")
	badSubPath := filepath.Join(projectsDir, project, badParentID, "subagents", "agent-bad.jsonl")

	mustWriteJSONL(t, topPath, []map[string]any{
		subagentJSONLRecord("2026-01-01T00:00:00Z", "req_top", "msg_top", "claude-x", "/home/x/proj1",
			usageMap(100, 10, 0, 0, 0)),
	})
	mustWriteJSONL(t, goodSubPath, []map[string]any{
		subagentJSONLRecord("2026-01-01T00:01:00Z", "req_sub", "msg_sub", "claude-haiku", "/home/x/proj1/backend",
			usageMap(50, 5, 0, 0, 0)),
	})
	mustWriteJSONL(t, badSubPath, []map[string]any{
		subagentJSONLRecord("2026-01-01T00:02:00Z", "req_bad", "msg_bad", "claude-haiku", "/home/x/proj1",
			usageMap(50, 5, 0, 0, 0)),
	})

	stats, err := Run(ctx, s, Options{ProjectsDir: projectsDir, MinFileSize: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.FilesSkippedBadParent != 1 {
		t.Errorf("FilesSkippedBadParent = %d, want 1", stats.FilesSkippedBadParent)
	}
	foundBadParentError := false
	for _, e := range stats.Errors {
		if strings.Contains(e, badParentID) {
			foundBadParentError = true
		}
	}
	if !foundBadParentError {
		t.Errorf("expected an error message naming the bad parent dir %q, got %v", badParentID, stats.Errors)
	}

	// The bad-parent subagent must never reach the turns table.
	var badCount int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_uuid = 'agent-bad'`).Scan(&badCount); err != nil {
		t.Fatalf("query bad subagent turns: %v", err)
	}
	if badCount != 0 {
		t.Errorf("agent-bad has %d turns, want 0 (should have been skipped, not written as garbage)", badCount)
	}

	// The valid subagent: session_uuid is its OWN filename stem (not the
	// parent's, which is what every record's "sessionId" field actually
	// holds, see parseFile), parent_session_uuid links back to the real
	// parent UUID, project rolls up under the parent's project, and cwd is
	// the record's own (which legitimately differs from the parent's).
	var (
		gotProject, gotParent, gotCwd string
	)
	err = s.DB.QueryRowContext(ctx, `
		SELECT project, parent_session_uuid, cwd FROM turns WHERE session_uuid = 'agent-good'
	`).Scan(&gotProject, &gotParent, &gotCwd)
	if err != nil {
		t.Fatalf("query good subagent turn: %v", err)
	}
	if gotProject != project {
		t.Errorf("subagent project = %q, want %q", gotProject, project)
	}
	if gotParent != parentUUID {
		t.Errorf("subagent parent_session_uuid = %q, want %q", gotParent, parentUUID)
	}
	if gotCwd != "/home/x/proj1/backend" {
		t.Errorf("subagent cwd = %q, want its own recorded cwd", gotCwd)
	}

	// The top-level session is unaffected: no parent, and its own cwd.
	var topParent, topCwd string
	err = s.DB.QueryRowContext(ctx, `
		SELECT parent_session_uuid, cwd FROM turns WHERE session_uuid = ?
	`, parentUUID).Scan(&topParent, &topCwd)
	if err != nil {
		t.Fatalf("query top-level turn: %v", err)
	}
	if topParent != "" {
		t.Errorf("top-level parent_session_uuid = %q, want empty", topParent)
	}
	if topCwd != "/home/x/proj1" {
		t.Errorf("top-level cwd = %q, want its own recorded cwd", topCwd)
	}
}

// TestRun_WorkflowAgentDiscoveryEndToEnd drives the real Run() pipeline
// against a workflow-agent transcript (one level deeper than a plain
// subagent) and its journal.jsonl sibling, covering the three things the
// spec calls out for this shape specifically: the agent file is ingested
// with parent_session_uuid/project/cwd set correctly despite the deeper
// nesting (the project derivation bug a fixed directory-depth assumption
// would have reintroduced here, see the comment on subagentContext), a
// workflow agent under a bad parent directory is skipped exactly like a
// bad-parent plain subagent, and journal.jsonl never contributes a turn
// because it is never even discovered as a transcript.
func TestRun_WorkflowAgentDiscoveryEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_STATE_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", dataDir)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.DB.Close()

	projectsDir := t.TempDir()
	const (
		parentUUID  = "a850d051-2b0d-455b-991c-a0a434be269f"
		badParentID = "not-a-uuid"
		project     = "proj1"
		wfID        = "wf_abc123"
	)

	goodWFPath := filepath.Join(projectsDir, project, parentUUID, "subagents", "workflows", wfID, "agent-good.jsonl")
	badWFPath := filepath.Join(projectsDir, project, badParentID, "subagents", "workflows", wfID, "agent-bad.jsonl")
	journalPath := filepath.Join(projectsDir, project, parentUUID, "subagents", "workflows", wfID, "journal.jsonl")

	mustWriteJSONL(t, goodWFPath, []map[string]any{
		subagentJSONLRecord("2026-01-01T00:03:00Z", "req_wf_good", "msg_wf_good", "claude-haiku", "/home/x/proj1/workflows",
			usageMap(70, 7, 0, 0, 0)),
	})
	mustWriteJSONL(t, badWFPath, []map[string]any{
		subagentJSONLRecord("2026-01-01T00:04:00Z", "req_wf_bad", "msg_wf_bad", "claude-haiku", "/home/x/proj1",
			usageMap(70, 7, 0, 0, 0)),
	})
	// journal.jsonl: not a transcript. Written with a shape that would
	// parse as an "assistant" record if parseFile ever saw it (it must
	// not), so this test would fail loudly rather than quietly if the glob
	// ever regressed to "*.jsonl".
	mustWriteJSONL(t, journalPath, []map[string]any{
		assistantRecord("2026-01-01T00:03:30Z", "req_journal", "msg_journal", "claude-haiku", usageMap(999, 999, 0, 0, 0)),
	})

	stats, err := Run(ctx, s, Options{ProjectsDir: projectsDir, MinFileSize: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.FilesSkippedBadParent != 1 {
		t.Errorf("FilesSkippedBadParent = %d, want 1", stats.FilesSkippedBadParent)
	}

	var (
		gotProject, gotParent, gotCwd string
	)
	err = s.DB.QueryRowContext(ctx, `
		SELECT project, parent_session_uuid, cwd FROM turns WHERE session_uuid = 'agent-good'
	`).Scan(&gotProject, &gotParent, &gotCwd)
	if err != nil {
		t.Fatalf("query good workflow agent turn: %v", err)
	}
	if gotProject != project {
		t.Errorf("workflow agent project = %q, want %q", gotProject, project)
	}
	if gotParent != parentUUID {
		t.Errorf("workflow agent parent_session_uuid = %q, want %q", gotParent, parentUUID)
	}
	if gotCwd != "/home/x/proj1/workflows" {
		t.Errorf("workflow agent cwd = %q, want its own recorded cwd", gotCwd)
	}

	var badCount int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_uuid = 'agent-bad'`).Scan(&badCount); err != nil {
		t.Fatalf("query bad workflow agent turns: %v", err)
	}
	if badCount != 0 {
		t.Errorf("agent-bad has %d turns, want 0 (bad parent dir should have been skipped)", badCount)
	}

	// The 999/999 usage in journal.jsonl would be unmistakable in the sums
	// below if it were ever ingested; confirm it never enters turns at all
	// (not under any session_uuid, since journal.jsonl never gets a
	// filename-stem identity of its own the way an agent file does).
	var journalCount int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE input_tokens = 999`).Scan(&journalCount); err != nil {
		t.Fatalf("query journal turns: %v", err)
	}
	if journalCount != 0 {
		t.Errorf("journal.jsonl contributed %d turns, want 0", journalCount)
	}
}

func mustWriteJSONL(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}
}

// TestRun_TwoConfigDirsAttributeToTheirOwnAccounts drives Run over the
// default dir and a CLAUDE_CONFIG_DIR-style second one, each logged in to a
// different account. Every turn must land on its own dir's account, and the
// default dir must take over the placeholder row that owns pre-accounts data.
func TestRun_TwoConfigDirsAttributeToTheirOwnAccounts(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_STATE_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", dataDir)
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctx := context.Background()
	s, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.DB.Close()

	personal := filepath.Join(home, ".claude")
	work := filepath.Join(home, ".claude-work")
	writeState := func(path, uuid string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"oauthAccount":{"accountUuid":"` + uuid + `","organizationUuid":"org-` + uuid + `"}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeState(filepath.Join(home, ".claude.json"), "acc-personal")
	writeState(filepath.Join(work, ".claude.json"), "acc-work")

	const (
		personalSession = "11111111-1111-1111-1111-111111111111"
		workSession     = "22222222-2222-2222-2222-222222222222"
	)
	mustWriteJSONL(t, filepath.Join(personal, "projects", "p", personalSession+".jsonl"), []map[string]any{
		assistantRecord("2026-01-01T00:00:00Z", "req_p", "msg_p", "claude-x", usageMap(100, 10, 0, 0, 0)),
	})
	mustWriteJSONL(t, filepath.Join(work, "projects", "p", workSession+".jsonl"), []map[string]any{
		assistantRecord("2026-01-01T00:00:00Z", "req_w", "msg_w", "claude-x", usageMap(100, 10, 0, 0, 0)),
	})

	stats, err := Run(ctx, s, Options{ConfigDirs: []string{personal, work}, MinFileSize: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stats.Errors) > 0 {
		t.Fatalf("Run errors: %v", stats.Errors)
	}
	if stats.Accounts[personal] != store.PlaceholderAccountID {
		t.Errorf("default dir account = %d, want the placeholder it claims", stats.Accounts[personal])
	}
	if stats.Accounts[work] == stats.Accounts[personal] || stats.Accounts[work] == 0 {
		t.Fatalf("work dir account = %d, want its own", stats.Accounts[work])
	}

	for session, want := range map[string]int64{
		personalSession: stats.Accounts[personal],
		workSession:     stats.Accounts[work],
	} {
		var got int64
		if err := s.DB.QueryRow(`SELECT account_id FROM turns WHERE session_uuid = ?`, session).Scan(&got); err != nil {
			t.Fatalf("turn for %s: %v", session, err)
		}
		if got != want {
			t.Errorf("session %s turn account = %d, want %d", session, got, want)
		}
	}
}
