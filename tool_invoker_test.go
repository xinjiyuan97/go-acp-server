package acpserver

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func newUnitInvoker(caps clientCapabilities) *runtimeToolInvoker {
	srv := NewServer(&fakeExecutor{reply: "ok"}, io.Discard, "test")
	srv.writer = newRPCWriter(io.Discard)
	return &runtimeToolInvoker{
		server:    srv,
		sessionID: "sid",
		cwd:       "/tmp",
		caps:      caps,
	}
}

func TestRuntimeToolInvokerInvokeToolErrorPaths(t *testing.T) {
	tests := []struct {
		name string
		caps clientCapabilities
		tool string
		args string
		want string
	}{
		{
			name: "read capability missing",
			caps: clientCapabilities{},
			tool: toolFSReadTextFile,
			args: `{"path":"a.txt"}`,
			want: "not available",
		},
		{
			name: "read invalid json",
			caps: clientCapabilities{FS: fileSystemCapabilities{ReadTextFile: true}},
			tool: toolFSReadTextFile,
			args: `{`,
			want: "invalid args",
		},
		{
			name: "write missing path",
			caps: clientCapabilities{FS: fileSystemCapabilities{WriteTextFile: true}},
			tool: toolFSWriteTextFile,
			args: `{"content":"x"}`,
			want: "path is required",
		},
		{
			name: "terminal create missing command",
			caps: clientCapabilities{Terminal: true},
			tool: toolTerminalCreate,
			args: `{}`,
			want: "command is required",
		},
		{
			name: "terminal output missing id",
			caps: clientCapabilities{Terminal: true},
			tool: toolTerminalOutput,
			args: `{}`,
			want: "terminalId is required",
		},
		{
			name: "terminal wait missing id",
			caps: clientCapabilities{Terminal: true},
			tool: toolTerminalWaitExit,
			args: `{}`,
			want: "terminalId is required",
		},
		{
			name: "terminal kill missing id",
			caps: clientCapabilities{Terminal: true},
			tool: toolTerminalKill,
			args: `{}`,
			want: "terminalId is required",
		},
		{
			name: "unknown tool",
			caps: clientCapabilities{},
			tool: "unknown_tool",
			args: `{}`,
			want: "unknown tool",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := newUnitInvoker(tc.caps)
			_, err := inv.InvokeTool(context.Background(), "", tc.tool, tc.args)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to contain %q, got %q", tc.want, err.Error())
			}
		})
	}
}

type writeAndTerminalExecutor struct{}

func (e *writeAndTerminalExecutor) StreamReply(
	ctx context.Context,
	_ []ChatTurn,
	_ string,
	tools RuntimeToolInvoker,
	onChunk func(chunk string) error,
) (string, error) {
	if _, err := tools.InvokeTool(ctx, "write-1", toolFSWriteTextFile, `{"path":"note.txt","content":"hello"}`); err != nil {
		return "", err
	}
	if _, err := tools.InvokeTool(ctx, "term-create", toolTerminalCreate, `{"command":"echo","args":["hi"],"cwd":"sub"}`); err != nil {
		return "", err
	}
	if _, err := tools.InvokeTool(ctx, "term-output", toolTerminalOutput, `{"terminalId":"term-1"}`); err != nil {
		return "", err
	}
	if _, err := tools.InvokeTool(ctx, "term-wait", toolTerminalWaitExit, `{"terminalId":"term-1"}`); err != nil {
		return "", err
	}
	if _, err := tools.InvokeTool(ctx, "term-kill", toolTerminalKill, `{"terminalId":"term-1"}`); err != nil {
		return "", err
	}
	if _, err := tools.InvokeTool(ctx, "term-release", toolTerminalRelease, `{"terminalId":"term-1"}`); err != nil {
		return "", err
	}

	reply := "done"
	if onChunk != nil {
		if err := onChunk(reply); err != nil {
			return "", err
		}
	}
	return reply, nil
}

func TestPromptInvokesWriteAndTerminalTools(t *testing.T) {
	var logs bytes.Buffer
	srv := NewServer(&writeAndTerminalExecutor{}, &logs, "test")

	initOut := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs": map[string]any{
					"writeTextFile": true,
				},
				"terminal": true,
			},
		},
	}))
	if len(initOut) != 1 {
		t.Fatalf("expected initialize response, got %v", initOut)
	}

	sessionID := createSession(t, srv)
	out := runServe(t, srv,
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "session/prompt",
			"params": map[string]any{
				"sessionId": sessionID,
				"prompt":    []map[string]any{{"type": "text", "text": "run all tools"}},
			},
		}),
		line(map[string]any{"jsonrpc": "2.0", "id": "acp-client-1", "result": nil}),
		line(map[string]any{"jsonrpc": "2.0", "id": "acp-client-2", "result": map[string]any{"terminalId": "term-1"}}),
		line(map[string]any{"jsonrpc": "2.0", "id": "acp-client-3", "result": map[string]any{"output": "hello", "truncated": false}}),
		line(map[string]any{"jsonrpc": "2.0", "id": "acp-client-4", "result": map[string]any{"exitCode": 0}}),
		line(map[string]any{"jsonrpc": "2.0", "id": "acp-client-5", "result": nil}),
		line(map[string]any{"jsonrpc": "2.0", "id": "acp-client-6", "result": nil}),
	)

	var seenPromptOK bool
	var seenWrite bool
	var seenCreate bool
	var seenOutput bool
	var seenWait bool
	var seenKill bool
	var seenRelease bool

	for _, raw := range out {
		var payload map[string]any
		mustUnmarshalLine(t, raw, &payload)

		if id, ok := payload["id"].(float64); ok && int(id) == 2 {
			result := payload["result"].(map[string]any)
			if got := result["stopReason"].(string); got == stopReasonEndTurn {
				seenPromptOK = true
			}
		}

		method, _ := payload["method"].(string)
		switch method {
		case methodFSWriteTextFile:
			seenWrite = true
			params := payload["params"].(map[string]any)
			if got := params["path"].(string); got != "/tmp/note.txt" {
				t.Fatalf("unexpected write path: %s", got)
			}
		case methodTerminalCreate:
			seenCreate = true
			params := payload["params"].(map[string]any)
			if got := params["cwd"].(string); got != "/tmp/sub" {
				t.Fatalf("unexpected terminal cwd: %s", got)
			}
		case methodTerminalOutput:
			seenOutput = true
		case methodTerminalWaitForExit:
			seenWait = true
		case methodTerminalKill:
			seenKill = true
		case methodTerminalRelease:
			seenRelease = true
		}
	}

	if !seenPromptOK {
		t.Fatalf("missing prompt response stopReason=end_turn: %v", out)
	}
	if !seenWrite || !seenCreate || !seenOutput || !seenWait || !seenKill || !seenRelease {
		t.Fatalf("missing one or more tool RPCs: %v", out)
	}
	if !strings.Contains(logs.String(), "[tool] done") {
		t.Fatalf("expected tool completion logs, got:\n%s", logs.String())
	}
}

func TestRuntimeToolInvokerHelpers(t *testing.T) {
	inv := newUnitInvoker(clientCapabilities{})

	if got := inv.resolvePath("  "); got != "" {
		t.Fatalf("expected empty path, got %q", got)
	}
	if got := inv.resolvePath("/tmp/../tmp/a.txt"); got != "/tmp/a.txt" {
		t.Fatalf("expected cleaned abs path, got %q", got)
	}
	if got := inv.resolvePath("a/b.txt"); got != "/tmp/a/b.txt" {
		t.Fatalf("expected cwd-relative path, got %q", got)
	}

	if got := decodeJSONValue(" "); got != nil {
		t.Fatalf("expected nil decode for empty json, got %#v", got)
	}
	if got := decodeJSONValue("{"); got != "{" {
		t.Fatalf("expected raw string for invalid json, got %#v", got)
	}
	if got := previewText("abcdef", 3); got != "abc...(truncated)" {
		t.Fatalf("unexpected previewText output: %q", got)
	}
}
