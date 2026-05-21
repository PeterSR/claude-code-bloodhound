package selfheal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// BridgeRequest is the wire shape between the MCP subcommand (claude's
// MCP server) and the daemon. The subcommand serialises whichever tool
// claude called; the daemon dispatches to the corresponding Session
// method.
type BridgeRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// BridgeResponse carries either a tool result (JSON-encoded) or an
// error message. Exactly one is populated per request.
type BridgeResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Err    string          `json:"err,omitempty"`
}

// Tool names. Single source of truth so subcommand + daemon can't
// drift on string spelling.
const (
	ToolReadPTY        = "read_pty"
	ToolSendKeys       = "send_keys"
	ToolTestRegex      = "test_regex"
	ToolSaveExtractor  = "save_extractor"
)

// BridgeServer accepts connections from a single MCP subcommand and
// dispatches tool calls to the wrapped Session. One server per heal.
type BridgeServer struct {
	session *Session
	ln      net.Listener
	path    string

	mu     sync.Mutex
	closed bool
}

// NewBridgeServer creates a server bound to a fresh unix socket inside
// $XDG_RUNTIME_DIR/bloodhound/. Returns the socket path so the parent
// can pass it to the subcommand via env / arg.
func NewBridgeServer(session *Session) (*BridgeServer, error) {
	dir, err := bridgeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Unique-ish path per heal so concurrent attempts don't collide
	// (defensive; we don't run them concurrently today).
	path := filepath.Join(dir, fmt.Sprintf("selfheal-%d.sock", os.Getpid()))
	_ = os.Remove(path) // stale from a previous crash
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &BridgeServer{session: session, ln: ln, path: path}, nil
}

// Path returns the unix-socket path the subcommand should dial.
func (s *BridgeServer) Path() string { return s.path }

// Close shuts the listener and removes the socket file. Safe to call
// repeatedly.
func (s *BridgeServer) Close() error {
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

// Serve accepts connections until Close is called or the listener
// errors out. Each connection handles a single MCP subcommand process
// for the duration of one heal; we don't multiplex across heals.
func (s *BridgeServer) Serve() error {
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

// handle reads line-delimited BridgeRequests, dispatches to the
// Session, writes line-delimited BridgeResponses. Connection lifecycle
// matches the subprocess that opened it.
func (s *BridgeServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Connection died unexpectedly; nothing to do.
			}
			return
		}
		var req BridgeRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = writeResp(w, BridgeResponse{Err: "decode request: " + err.Error()})
			continue
		}
		resp := s.dispatch(req)
		if err := writeResp(w, resp); err != nil {
			return
		}
	}
}

func (s *BridgeServer) dispatch(req BridgeRequest) BridgeResponse {
	switch req.Tool {
	case ToolReadPTY:
		var args ReadPTYRequest
		if len(req.Args) > 0 {
			if err := json.Unmarshal(req.Args, &args); err != nil {
				return BridgeResponse{Err: "decode read_pty args: " + err.Error()}
			}
		}
		return ok(s.session.ReadPTY(args))
	case ToolSendKeys:
		var args SendKeysRequest
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return BridgeResponse{Err: "decode send_keys args: " + err.Error()}
		}
		res, err := s.session.SendKeys(args)
		if err != nil {
			return BridgeResponse{Err: err.Error()}
		}
		return ok(res)
	case ToolTestRegex:
		var args TestRegexRequest
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return BridgeResponse{Err: "decode test_regex args: " + err.Error()}
		}
		return ok(s.session.TestRegex(args))
	case ToolSaveExtractor:
		var args SaveExtractorRequest
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return BridgeResponse{Err: "decode save_extractor args: " + err.Error()}
		}
		return ok(s.session.SaveExtractor(args))
	default:
		return BridgeResponse{Err: "unknown tool: " + req.Tool}
	}
}

func ok(v any) BridgeResponse {
	b, err := json.Marshal(v)
	if err != nil {
		return BridgeResponse{Err: "encode result: " + err.Error()}
	}
	return BridgeResponse{Result: b}
}

func writeResp(w *bufio.Writer, resp BridgeResponse) error {
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

// BridgeClient is the subcommand-side handle to talk to a running
// BridgeServer. One-call-at-a-time; the MCP server above it serialises
// tool invocations already.
type BridgeClient struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	mu   sync.Mutex
}

// DialBridge opens a connection to a BridgeServer at path. Caller
// closes when done.
func DialBridge(path string) (*BridgeClient, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial bridge: %w", err)
	}
	return &BridgeClient{
		conn: conn,
		r:    bufio.NewReader(conn),
		w:    bufio.NewWriter(conn),
	}, nil
}

// Close drops the bridge connection.
func (c *BridgeClient) Close() error { return c.conn.Close() }

// Call sends one tool request, waits for the response. Generic over the
// result type via json.RawMessage; subcommand handlers decode into
// the right shape.
func (c *BridgeClient) Call(tool string, args any) (json.RawMessage, error) {
	argBytes, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode args: %w", err)
	}
	req := BridgeRequest{Tool: tool, Args: argBytes}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(append(reqBytes, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	if err := c.w.Flush(); err != nil {
		return nil, fmt.Errorf("flush request: %w", err)
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp BridgeResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp.Result, nil
}

// bridgeDir returns $XDG_RUNTIME_DIR/bloodhound (created on demand by
// callers). Shared with the api socket path resolver.
func bridgeDir() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set; cannot place bridge socket")
	}
	return filepath.Join(dir, "bloodhound"), nil
}
