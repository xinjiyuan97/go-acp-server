package acpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestHandlerErrorBranches(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")

	out := runServe(t, srv,
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "unknown/method",
			"params":  map[string]any{},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "initialize",
			"params":  "invalid",
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"method":  "authenticate",
			"params":  "invalid",
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      4,
			"method":  "session/new",
			"params":  map[string]any{},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      5,
			"method":  "session/new",
			"params": map[string]any{
				"cwd": "relative/path",
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      6,
			"method":  "session/load",
			"params": map[string]any{
				"cwd": "/tmp",
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      7,
			"method":  "session/list",
			"params": map[string]any{
				"cursor": "abc",
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      8,
			"method":  "session/list",
			"params": map[string]any{
				"cwd": "relative/path",
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      9,
			"method":  "session/prompt",
			"params": map[string]any{
				"sessionId": "missing-session",
				"prompt":    []map[string]any{{"type": "text", "text": "hi"}},
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/cancel",
			"params":  "invalid",
		}),
	)

	if len(out) != 9 {
		t.Fatalf("expected 9 responses, got %d: %v", len(out), out)
	}

	var methodNotFound bool
	for _, raw := range out {
		var payload map[string]any
		mustUnmarshalLine(t, raw, &payload)
		errObj := payload["error"].(map[string]any)
		code := int(errObj["code"].(float64))
		if code == errMethodNotFound {
			methodNotFound = true
		}
		if code != errMethodNotFound && code != errInvalidParams {
			t.Fatalf("unexpected error code: %d payload=%v", code, payload)
		}
	}
	if !methodNotFound {
		t.Fatalf("expected method-not-found error in outputs: %v", out)
	}
}

func TestCallClientContextCanceledAndRPCWriterMarshalError(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")
	var out bytes.Buffer
	srv.writer = newRPCWriter(&out)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := srv.callClient(ctx, methodFSReadTextFile, readTextFileRequest{
		SessionID: "sid",
		Path:      "/tmp/a.txt",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled error, got %v", err)
	}

	rw := newRPCWriter(io.Discard)
	if err := rw.write(make(chan int)); err == nil {
		t.Fatalf("expected marshal error when writing channel payload")
	}
}

func TestHandleClientResponseWithNonKeyID(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")

	srv.handleClientResponse(incomingMessage{ID: json.RawMessage(`{"x":1}`)})
	srv.pendingMu.Lock()
	defer srv.pendingMu.Unlock()
	if len(srv.earlyResponses) != 1 {
		t.Fatalf("expected early response to be queued, got %+v", srv.earlyResponses)
	}
}
