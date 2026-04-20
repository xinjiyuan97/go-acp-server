package acpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestIncomingMessageHasID(t *testing.T) {
	if (incomingMessage{}).hasID() {
		t.Fatalf("expected empty message to have no id")
	}
	if !(incomingMessage{ID: json.RawMessage(`"abc"`)}).hasID() {
		t.Fatalf("expected non-empty id to be detected")
	}
}

func TestDecodeParamsAndTimeFormatHelpers(t *testing.T) {
	type params struct {
		Name string `json:"name"`
	}

	var p params
	if err := decodeParams(nil, &p); err != nil {
		t.Fatalf("decodeParams(nil) failed: %v", err)
	}

	if err := decodeParams(json.RawMessage(`{"name":"alice"}`), &p); err != nil {
		t.Fatalf("decodeParams valid payload failed: %v", err)
	}
	if p.Name != "alice" {
		t.Fatalf("unexpected decoded value: %+v", p)
	}

	if err := decodeParams(json.RawMessage(`{`), &p); err == nil {
		t.Fatalf("expected decodeParams to fail on invalid json")
	}

	if got := formatRFC3339UTC(time.Time{}); got != "" {
		t.Fatalf("expected zero time format to be empty, got %q", got)
	}
}

func TestPromptUpdateWriterValidationAndContext(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, io.Discard, "test")
	var out bytes.Buffer
	srv.writer = newRPCWriter(&out)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newPromptUpdateWriter(srv, "sid", ctx, nil)
	if err := w.Raw(map[string]any{"sessionUpdate": "plan"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}

	w = newPromptUpdateWriter(srv, "sid", context.Background(), nil)
	if err := w.Raw(nil); err == nil || !strings.Contains(err.Error(), "payload is required") {
		t.Fatalf("expected payload-required error, got %v", err)
	}
	if err := w.Raw(map[string]any{}); err == nil || !strings.Contains(err.Error(), "sessionUpdate is required") {
		t.Fatalf("expected sessionUpdate-required error, got %v", err)
	}
	if err := w.CurrentMode(" "); err == nil {
		t.Fatalf("expected current mode validation error")
	}
	if err := w.Raw(map[string]any{"sessionUpdate": "plan", "entries": []any{}}); err != nil {
		t.Fatalf("expected valid raw update, got %v", err)
	}
	if !strings.Contains(out.String(), `"method":"session/update"`) {
		t.Fatalf("expected session/update output, got %s", out.String())
	}
}

func TestServerUtilityFunctions(t *testing.T) {
	if got := normalizeID(nil); string(got) != "null" {
		t.Fatalf("expected normalizeID(nil)=null, got %s", string(got))
	}
	if got := normalizeID(json.RawMessage(`"x"`)); string(got) != `"x"` {
		t.Fatalf("unexpected normalizeID value: %s", string(got))
	}

	if got := rpcIDKey(json.RawMessage(`"abc"`)); got != "s:abc" {
		t.Fatalf("unexpected string rpc id key: %s", got)
	}
	if got := rpcIDKey(json.RawMessage(`12`)); got != "n:12" {
		t.Fatalf("unexpected numeric rpc id key: %s", got)
	}
	if got := rpcIDKey(json.RawMessage(`true`)); got != "b:true" {
		t.Fatalf("unexpected bool rpc id key: %s", got)
	}
	if got := rpcIDKey(json.RawMessage(`{`)); !strings.HasPrefix(got, "j:") {
		t.Fatalf("expected fallback json key, got %s", got)
	}

	if got := compactJSON(nil); got != "null" {
		t.Fatalf("expected compactJSON(nil)=null, got %q", got)
	}
	if got := compactJSON(make(chan int)); !strings.Contains(got, "marshal_error") {
		t.Fatalf("expected marshal error summary, got %q", got)
	}
	if got := compactRawJSON(nil); got != "null" {
		t.Fatalf("expected compactRawJSON(nil)=null, got %q", got)
	}
	if got := compactRawJSON(json.RawMessage(`{"ok":true}`)); got != `{"ok":true}` {
		t.Fatalf("unexpected compactRawJSON output: %q", got)
	}

	if got := newSessionID(); !strings.HasPrefix(got, "eino-") {
		t.Fatalf("expected eino- prefix, got %q", got)
	}

	srv := NewServer(&fakeExecutor{reply: "ok"}, io.Discard, "")
	if got := srv.buildVersion(); got != "dev" {
		t.Fatalf("expected default version dev, got %q", got)
	}
}

func TestCallClientEarlyResponseAndClientError(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, io.Discard, "test")
	var out bytes.Buffer
	srv.writer = newRPCWriter(&out)

	srv.handleClientResponse(incomingMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"acp-client-1"`),
		Result:  json.RawMessage(`{"content":"from-early"}`),
	})

	var readOut readTextFileResponse
	if err := srv.callClient(context.Background(), methodFSReadTextFile, readTextFileRequest{
		SessionID: "sid",
		Path:      "/tmp/a.txt",
	}, &readOut); err != nil {
		t.Fatalf("callClient with early response failed: %v", err)
	}
	if readOut.Content != "from-early" {
		t.Fatalf("unexpected early response decode: %+v", readOut)
	}

	srv.handleClientResponse(incomingMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"acp-client-2"`),
		Error:   &rpcError{Code: 123, Message: "denied"},
	})
	if err := srv.callClient(context.Background(), methodFSReadTextFile, map[string]any{
		"sessionId": "sid",
		"path":      "/tmp/b.txt",
	}, nil); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected client error propagation, got %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestWriteResultAndWriteErrorLogFailures(t *testing.T) {
	var logs bytes.Buffer
	srv := NewServer(&fakeExecutor{reply: "ok"}, &logs, "test")
	srv.writer = newRPCWriter(failingWriter{})

	srv.writeResult(json.RawMessage(`1`), map[string]any{"ok": true})
	srv.writeError(json.RawMessage(`1`), errInternalError, "boom")

	text := logs.String()
	if !strings.Contains(text, "failed to write success response") {
		t.Fatalf("expected success write failure log, got: %s", text)
	}
	if !strings.Contains(text, "failed to write error response") {
		t.Fatalf("expected error write failure log, got: %s", text)
	}
}

func TestAuthenticateRequest(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "authenticate",
		"params": map[string]any{
			"methodId": "none",
		},
	}))
	if len(out) != 1 {
		t.Fatalf("expected one authenticate response, got %v", out)
	}

	var payload map[string]any
	mustUnmarshalLine(t, out[0], &payload)
	if _, ok := payload["result"].(map[string]any); !ok {
		t.Fatalf("authenticate result missing object: %v", payload)
	}
}
