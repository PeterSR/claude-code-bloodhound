package trail

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// bridgeServer hosts the single save_trail_brief tool over a unix
// socket, speaking the same line-delimited protocol as the self-heal
// bridge (so the shared relay subcommand can talk to it via
// selfheal.DialBridge). One server per session analysis.
type bridgeServer struct {
	ln   net.Listener
	path string

	mu       sync.Mutex
	closed   bool
	captured *SaveBriefArgs

	savedOnce sync.Once
	savedCh   chan struct{}
}

func newBridgeServer() (*bridgeServer, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return nil, errors.New("XDG_RUNTIME_DIR is not set; cannot place trail bridge socket")
	}
	dir = filepath.Join(dir, "bloodhound")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("trail-%d.sock", os.Getpid()))
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &bridgeServer{ln: ln, path: path, savedCh: make(chan struct{})}, nil
}

// Saved closes once save_trail_brief has been called successfully.
func (s *bridgeServer) Saved() <-chan struct{} { return s.savedCh }

func (s *bridgeServer) Path() string { return s.path }

// Brief returns the captured args (nil until the tool fires).
func (s *bridgeServer) Brief() *SaveBriefArgs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captured
}

func (s *bridgeServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

func (s *bridgeServer) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handle(conn)
	}
}

func (s *bridgeServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req selfheal.BridgeRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = writeResp(w, selfheal.BridgeResponse{Err: "decode request: " + err.Error()})
			continue
		}
		_ = writeResp(w, s.dispatch(req))
	}
}

func (s *bridgeServer) dispatch(req selfheal.BridgeRequest) selfheal.BridgeResponse {
	if req.Tool != ToolSaveBrief {
		return selfheal.BridgeResponse{Err: "unknown tool: " + req.Tool}
	}
	var args SaveBriefArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return selfheal.BridgeResponse{Err: "decode save_trail_brief args: " + err.Error()}
	}
	s.mu.Lock()
	s.captured = &args
	s.mu.Unlock()
	s.savedOnce.Do(func() { close(s.savedCh) })
	out, _ := json.Marshal(map[string]any{"ok": true})
	return selfheal.BridgeResponse{Result: out}
}

func writeResp(w *bufio.Writer, resp selfheal.BridgeResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}
