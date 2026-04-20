package connections

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	acpserver "github.com/xinjiyuan97/go-acp-server"
)

type testExecutor struct{}

func (e *testExecutor) StreamReply(
	_ context.Context,
	_ []acpserver.ContentBlock,
	_ acpserver.RuntimeToolInvoker,
	onChunk func(chunk string) error,
) (string, error) {
	reply := "ok"
	if onChunk != nil {
		_ = onChunk(reply)
	}
	return reply, nil
}

func jsonLine(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEndpointServeNil(t *testing.T) {
	var endpoint *Endpoint
	if err := endpoint.Serve(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Fatalf("expected nil endpoint serve to fail")
	}

	endpoint = NewEndpoint(nil)
	if err := endpoint.Serve(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Fatalf("expected nil server endpoint serve to fail")
	}
}

func TestHTTPHandler(t *testing.T) {
	server := acpserver.NewServer(&testExecutor{}, io.Discard, "test")
	endpoint := NewEndpoint(server)
	handler := NewHTTPHandler(endpoint, HTTPHandlerOptions{})

	req := httptest.NewRequest(http.MethodPost, "/acp", strings.NewReader(
		jsonLine(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": 1,
			},
		})+"\n",
	))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/x-ndjson") {
		t.Fatalf("unexpected content type: %s", got)
	}
	if !strings.Contains(rec.Body.String(), `"protocolVersion":1`) {
		t.Fatalf("missing initialize result: %s", rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/acp", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", getRec.Code)
	}

	nilHandler := NewHTTPHandler(nil, HTTPHandlerOptions{})
	nilReq := httptest.NewRequest(http.MethodPost, "/acp", strings.NewReader(
		jsonLine(map[string]any{
			"jsonrpc": "2.0",
			"id":      10,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": 1,
			},
		})+"\n",
	))
	nilRec := httptest.NewRecorder()
	nilHandler.ServeHTTP(nilRec, nilReq)
	if nilRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for nil endpoint, got %d", nilRec.Code)
	}
}

type fakeWSConn struct {
	once   sync.Once
	inOnce sync.Once
	inCh   chan string
	outMu  sync.Mutex
	out    []string
	closed bool
}

func newFakeWSConn() *fakeWSConn {
	return &fakeWSConn{
		inCh: make(chan string, 8),
	}
}

func (c *fakeWSConn) ReadText(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case msg, ok := <-c.inCh:
		if !ok {
			return "", io.EOF
		}
		return msg, nil
	}
}

func (c *fakeWSConn) WriteText(_ context.Context, text string) error {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.out = append(c.out, text)
	return nil
}

func (c *fakeWSConn) Close() error {
	c.once.Do(func() {
		c.outMu.Lock()
		c.closed = true
		c.outMu.Unlock()
		c.inOnce.Do(func() {
			close(c.inCh)
		})
	})
	return nil
}

func (c *fakeWSConn) closeInput() {
	c.inOnce.Do(func() {
		close(c.inCh)
	})
}

func TestServeWebSocket(t *testing.T) {
	server := acpserver.NewServer(&testExecutor{}, io.Discard, "test")
	endpoint := NewEndpoint(server)
	conn := newFakeWSConn()

	conn.inCh <- jsonLine(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
		},
	})
	conn.inCh <- jsonLine(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/new",
		"params": map[string]any{
			"cwd": "/tmp",
		},
	})
	conn.closeInput()

	if err := ServeWebSocket(context.Background(), endpoint, conn); err != nil {
		t.Fatalf("ServeWebSocket failed: %v", err)
	}

	conn.outMu.Lock()
	defer conn.outMu.Unlock()
	if len(conn.out) != 2 {
		t.Fatalf("expected 2 websocket outputs, got %d: %v", len(conn.out), conn.out)
	}
	for _, raw := range conn.out {
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("invalid websocket output line %q: %v", raw, err)
		}
		if payload["error"] != nil {
			t.Fatalf("unexpected error payload: %v", payload)
		}
	}
}

func TestServeWebSocketNilArgsAndCloseError(t *testing.T) {
	if err := ServeWebSocket(context.Background(), nil, nil); err == nil {
		t.Fatalf("expected nil endpoint error")
	}

	server := acpserver.NewServer(&testExecutor{}, io.Discard, "test")
	endpoint := NewEndpoint(server)
	if err := ServeWebSocket(context.Background(), endpoint, nil); err == nil {
		t.Fatalf("expected nil conn error")
	}

	if !isConnClosedError(io.EOF) {
		t.Fatalf("expected io.EOF treated as closed")
	}
	if !isConnClosedError(net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed treated as closed")
	}
	if isConnClosedError(context.DeadlineExceeded) {
		t.Fatalf("deadline exceeded should not be treated as closed")
	}
}
