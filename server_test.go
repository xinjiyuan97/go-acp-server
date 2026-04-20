package acpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

type fakeExecutor struct {
	reply string
	err   error

	mu          sync.Mutex
	lastPrompt  []ContentBlock
	lastPromptT string
}

func (f *fakeExecutor) StreamReply(
	_ context.Context,
	prompt []ContentBlock,
	_ RuntimeToolInvoker,
	_ PromptUpdateWriter,
) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.mu.Lock()
	f.lastPrompt = cloneContentBlocks(prompt)
	f.lastPromptT = extractPromptText(prompt)
	f.mu.Unlock()
	return f.reply, nil
}

func (f *fakeExecutor) prompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPromptT
}

func (f *fakeExecutor) promptBlocks() []ContentBlock {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneContentBlocks(f.lastPrompt)
}

type contextCaptureExecutor struct {
	mu   sync.Mutex
	meta SessionRuntimeContext
	ok   bool
}

func (e *contextCaptureExecutor) StreamReply(
	ctx context.Context,
	_ []ContentBlock,
	_ RuntimeToolInvoker,
	_ PromptUpdateWriter,
) (string, error) {
	meta, ok := SessionRuntimeContextFromContext(ctx)
	e.mu.Lock()
	e.meta = meta
	e.ok = ok
	e.mu.Unlock()
	return "ok", nil
}

func (e *contextCaptureExecutor) snapshot() (SessionRuntimeContext, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.meta, e.ok
}

func TestInitialize(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test-version")

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
		},
	}))

	if len(out) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(out), out)
	}

	var payload map[string]any
	mustUnmarshalLine(t, out[0], &payload)

	result := payload["result"].(map[string]any)
	if got := int(result["protocolVersion"].(float64)); got != 1 {
		t.Fatalf("unexpected protocolVersion: %d", got)
	}

	agentInfo := result["agentInfo"].(map[string]any)
	if got := agentInfo["version"].(string); got != "test-version" {
		t.Fatalf("unexpected agentInfo.version: %s", got)
	}
}

func TestPromptEmitsSessionUpdate(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "hello from eino"}, nil, "test")

	sessionID := createSession(t, srv)

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": "hi"},
			},
		},
	}))

	if len(out) < 4 {
		t.Fatalf("expected at least 4 output lines (updates + response), got %d: %v", len(out), out)
	}

	var userChunkUpdateFound bool
	var chunkUpdateFound bool
	var infoUpdateFound bool
	var modeUpdateFound bool
	var commandsUpdateFound bool
	var configUpdateFound bool
	var resp map[string]any
	for _, raw := range out {
		var payload map[string]any
		mustUnmarshalLine(t, raw, &payload)
		method, hasMethod := payload["method"].(string)
		if hasMethod && method == "session/update" {
			params := payload["params"].(map[string]any)
			update := params["update"].(map[string]any)
			switch update["sessionUpdate"] {
			case "user_message_chunk":
				content := update["content"].(map[string]any)
				if got := content["text"].(string); got != "hi" {
					t.Fatalf("unexpected user chunk text: %s", got)
				}
				userChunkUpdateFound = true
			case "agent_message_chunk":
				content := update["content"].(map[string]any)
				if got := content["text"].(string); got != "hello from eino" {
					t.Fatalf("unexpected chunk text: %s", got)
				}
				chunkUpdateFound = true
			case "session_info_update":
				if _, ok := update["updatedAt"].(string); !ok {
					t.Fatalf("session_info_update missing updatedAt: %v", update)
				}
				infoUpdateFound = true
			case "current_mode_update":
				if got, _ := update["currentModeId"].(string); got == "" {
					t.Fatalf("current_mode_update missing currentModeId: %v", update)
				}
				modeUpdateFound = true
			case "available_commands_update":
				if _, ok := update["availableCommands"].([]any); !ok {
					t.Fatalf("available_commands_update missing availableCommands: %v", update)
				}
				commandsUpdateFound = true
			case "config_option_update":
				if _, ok := update["configOptions"].([]any); !ok {
					t.Fatalf("config_option_update missing configOptions: %v", update)
				}
				configUpdateFound = true
			default:
				t.Fatalf("unexpected sessionUpdate: %v", update["sessionUpdate"])
			}
			continue
		}
		resp = payload
	}

	if !userChunkUpdateFound {
		t.Fatalf("missing user_message_chunk update: %v", out)
	}
	if !chunkUpdateFound {
		t.Fatalf("missing agent_message_chunk update: %v", out)
	}
	if !infoUpdateFound {
		t.Fatalf("missing session_info_update update: %v", out)
	}
	if !modeUpdateFound {
		t.Fatalf("missing current_mode_update update: %v", out)
	}
	if !commandsUpdateFound {
		t.Fatalf("missing available_commands_update update: %v", out)
	}
	if !configUpdateFound {
		t.Fatalf("missing config_option_update update: %v", out)
	}

	result := resp["result"].(map[string]any)
	if got := result["stopReason"].(string); got != "end_turn" {
		t.Fatalf("unexpected stopReason: %s", got)
	}
}

func TestPromptContextIncludesSessionRuntimeMetadata(t *testing.T) {
	exec := &contextCaptureExecutor{}
	srv := NewServer(exec, nil, "test")

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "session/new",
		"params": map[string]any{
			"cwd": "/tmp",
			"mcpServers": []map[string]any{
				{"type": "stdio", "name": "mcp-demo", "command": "mcp", "args": []string{"--x"}},
			},
		},
	}))
	if len(out) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(out), out)
	}

	var payload map[string]any
	mustUnmarshalLine(t, out[0], &payload)
	sessionID := payload["result"].(map[string]any)["sessionId"].(string)

	_ = runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
		},
	}))

	meta, ok := exec.snapshot()
	if !ok {
		t.Fatalf("executor did not receive session runtime context")
	}
	if meta.SessionID != sessionID {
		t.Fatalf("unexpected session id in context: %q", meta.SessionID)
	}
	if meta.CWD != "/tmp" {
		t.Fatalf("unexpected cwd in context: %q", meta.CWD)
	}
	if len(meta.MCPServers) != 1 || meta.MCPServers[0].Name != "mcp-demo" {
		t.Fatalf("unexpected mcp servers in context: %+v", meta.MCPServers)
	}
}

type richUpdateExecutor struct{}

func (e *richUpdateExecutor) StreamReply(
	_ context.Context,
	_ []ContentBlock,
	_ RuntimeToolInvoker,
	updates PromptUpdateWriter,
) (string, error) {
	if err := updates.AgentThoughtChunk("thinking..."); err != nil {
		return "", err
	}
	if err := updates.Plan([]PlanEntryUpdate{
		{
			Content:  "Write tests for stream events",
			Priority: "high",
			Status:   "in_progress",
		},
	}); err != nil {
		return "", err
	}
	if err := updates.AvailableCommands([]AvailableCommandUpdate{
		{Name: "rename", Description: "Rename current session"},
	}); err != nil {
		return "", err
	}
	if err := updates.CurrentMode("default"); err != nil {
		return "", err
	}
	if err := updates.ConfigOptions([]SessionConfigOptionUpdate{
		{
			Name:        "reasoning_effort",
			Description: "Reasoning effort level",
			Category:    "model",
			Value:       "medium",
		},
	}); err != nil {
		return "", err
	}
	reply := "rich reply"
	if err := updates.AgentMessageChunk(reply); err != nil {
		return "", err
	}
	return reply, nil
}

func TestPromptWithRichUpdateExecutor(t *testing.T) {
	srv := NewServer(&richUpdateExecutor{}, nil, "test")
	sessionID := createSession(t, srv)

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      21,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": "plan this"},
			},
		},
	}))

	var seen = map[string]bool{}
	var promptResp map[string]any
	for _, raw := range out {
		var payload map[string]any
		mustUnmarshalLine(t, raw, &payload)

		if method, ok := payload["method"].(string); ok && method == "session/update" {
			params := payload["params"].(map[string]any)
			update := params["update"].(map[string]any)
			kind, _ := update["sessionUpdate"].(string)
			seen[kind] = true
			continue
		}
		promptResp = payload
	}

	for _, required := range []string{
		"user_message_chunk",
		"agent_thought_chunk",
		"plan",
		"available_commands_update",
		"current_mode_update",
		"config_option_update",
		"agent_message_chunk",
		"session_info_update",
	} {
		if !seen[required] {
			t.Fatalf("missing %s update in output: %v", required, out)
		}
	}

	result := promptResp["result"].(map[string]any)
	if got := result["stopReason"].(string); got != "end_turn" {
		t.Fatalf("unexpected stopReason: %s", got)
	}
}

func TestCancelBeforePromptReturnsCancelled(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "should not be used"}, nil, "test")
	sessionID := createSession(t, srv)

	out := runServe(t, srv,
		line(map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/cancel",
			"params": map[string]any{
				"sessionId": sessionID,
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"method":  "session/prompt",
			"params": map[string]any{
				"sessionId": sessionID,
				"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
			},
		}),
	)

	if len(out) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(out), out)
	}

	var resp map[string]any
	mustUnmarshalLine(t, out[0], &resp)
	result := resp["result"].(map[string]any)
	if got := result["stopReason"].(string); got != "cancelled" {
		t.Fatalf("unexpected stopReason: %s", got)
	}
}

func TestPromptWithoutTextIsAccepted(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")
	sessionID := createSession(t, srv)

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "image", "data": "abc"}},
		},
	}))

	var resp map[string]any
	for _, raw := range out {
		var payload map[string]any
		mustUnmarshalLine(t, raw, &payload)
		if id, ok := payload["id"].(float64); ok && int(id) == 4 {
			resp = payload
			break
		}
	}
	if resp == nil {
		t.Fatalf("missing session/prompt response: %v", out)
	}
	result := resp["result"].(map[string]any)
	if got := result["stopReason"].(string); got != "end_turn" {
		t.Fatalf("unexpected stopReason: %s", got)
	}
}

func TestPromptPassesImageBlockToExecutor(t *testing.T) {
	exec := &fakeExecutor{reply: "ok"}
	srv := NewServer(exec, nil, "test")
	sessionID := createSession(t, srv)

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      41,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "image", "mimeType": "image/png", "data": "ZmFrZQ==", "uri": "file:///tmp/x.png"},
			},
		},
	}))

	var foundResponse bool
	for _, raw := range out {
		var payload map[string]any
		mustUnmarshalLine(t, raw, &payload)
		if id, ok := payload["id"].(float64); ok && int(id) == 41 {
			result := payload["result"].(map[string]any)
			if got := result["stopReason"].(string); got != "end_turn" {
				t.Fatalf("unexpected stopReason: %s", got)
			}
			foundResponse = true
		}
	}
	if !foundResponse {
		t.Fatalf("missing session/prompt response: %v", out)
	}

	blocks := exec.promptBlocks()
	if len(blocks) != 1 {
		t.Fatalf("expected one prompt block, got %+v", blocks)
	}
	if got, _ := blocks[0]["type"].(string); got != "image" {
		t.Fatalf("expected image block type, got %+v", blocks[0]["type"])
	}
	if got, _ := blocks[0]["mimeType"].(string); got != "image/png" {
		t.Fatalf("expected image mimeType, got %+v", blocks[0]["mimeType"])
	}
	if got, _ := blocks[0]["data"].(string); got != "ZmFrZQ==" {
		t.Fatalf("expected image data to pass through, got %+v", blocks[0]["data"])
	}
}

func TestPromptPassesUserTextWithoutServerPreface(t *testing.T) {
	exec := &fakeExecutor{reply: "ok"}
	srv := NewServer(exec, nil, "test")

	out := runServe(t, srv,
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{
						"readTextFile":  true,
						"writeTextFile": true,
					},
					"terminal": true,
				},
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "session/new",
			"params": map[string]any{
				"cwd": "/tmp",
				"mcpServers": []map[string]any{
					{
						"type": "stdio",
						"name": "demo-mcp",
					},
				},
			},
		}),
	)

	if len(out) != 2 {
		t.Fatalf("expected 2 output lines, got %d: %v", len(out), out)
	}

	var sessionID string
	for _, line := range out {
		var payload map[string]any
		mustUnmarshalLine(t, line, &payload)

		id, ok := payload["id"].(float64)
		if !ok || int(id) != 2 {
			continue
		}

		result, ok := payload["result"].(map[string]any)
		if !ok {
			t.Fatalf("session/new response missing result: %v", payload)
		}
		sessionID, _ = result["sessionId"].(string)
	}
	if sessionID == "" {
		t.Fatalf("failed to find session/new response in output: %v", out)
	}

	promptOut := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
		},
	}))

	if len(promptOut) < 2 {
		t.Fatalf("expected at least 2 output lines, got %d: %v", len(promptOut), promptOut)
	}

	p := exec.prompt()
	if got := strings.TrimSpace(p); got != "hello" {
		t.Fatalf("prompt should be raw user text, got: %q", got)
	}
}

type toolCallingExecutor struct{}

func (e *toolCallingExecutor) StreamReply(
	ctx context.Context,
	_ []ContentBlock,
	tools RuntimeToolInvoker,
	_ PromptUpdateWriter,
) (string, error) {
	result, err := tools.InvokeTool(ctx, "call-1", toolFSReadTextFile, `{"path":"a.txt"}`)
	if err != nil {
		return "", err
	}
	reply := "read ok: " + result
	return reply, nil
}

func TestPromptCanInvokeClientReadTextFile(t *testing.T) {
	var logs bytes.Buffer
	srv := NewServer(&toolCallingExecutor{}, &logs, "test")

	out := runServe(t, srv,
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": 1,
				"clientCapabilities": map[string]any{
					"fs": map[string]any{
						"readTextFile": true,
					},
				},
			},
		}),
	)

	if len(out) != 1 {
		t.Fatalf("expected 1 output line from initialize, got: %v", out)
	}
	sessionID := createSession(t, srv)
	out = runServe(t, srv,
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"method":  "session/prompt",
			"params": map[string]any{
				"sessionId": sessionID,
				"prompt":    []map[string]any{{"type": "text", "text": "please read file"}},
			},
		}),
		line(map[string]any{
			"jsonrpc": "2.0",
			"id":      "acp-client-1",
			"result": map[string]any{
				"content": "from-client",
			},
		}),
	)

	var foundFSRead bool
	var foundToolCreate bool
	var foundToolUpdate bool
	var foundPromptResult bool
	for _, l := range out {
		var msg map[string]any
		mustUnmarshalLine(t, l, &msg)

		if method, ok := msg["method"].(string); ok && method == "fs/read_text_file" {
			foundFSRead = true
			params := msg["params"].(map[string]any)
			if got := params["path"].(string); got != "/tmp/a.txt" {
				t.Fatalf("unexpected fs/read_text_file path: %s", got)
			}
		}

		if method, ok := msg["method"].(string); ok && method == "session/update" {
			update := msg["params"].(map[string]any)["update"].(map[string]any)
			switch update["sessionUpdate"] {
			case "tool_call":
				foundToolCreate = true
			case "tool_call_update":
				foundToolUpdate = true
			}
		}

		if id, ok := msg["id"].(float64); ok && int(id) == 3 {
			result := msg["result"].(map[string]any)
			if result["stopReason"].(string) == "end_turn" {
				foundPromptResult = true
			}
		}
	}

	if !foundFSRead {
		t.Fatalf("expected outbound fs/read_text_file request, got: %v", out)
	}
	if !foundToolCreate || !foundToolUpdate {
		t.Fatalf("expected tool_call + tool_call_update session updates, got: %v", out)
	}
	if !foundPromptResult {
		t.Fatalf("missing prompt response stopReason=end_turn: %v", out)
	}
	if !strings.Contains(logs.String(), "[tool] start") {
		t.Fatalf("expected tool start log, got logs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "[acp->client] method=fs/read_text_file") {
		t.Fatalf("expected outbound fs/read log, got logs:\n%s", logs.String())
	}
}

func TestLoadSession(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")
	sessionID := createSession(t, srv)

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      11,
		"method":  "session/load",
		"params": map[string]any{
			"sessionId": sessionID,
			"cwd":       "/tmp/loaded",
			"mcpServers": []map[string]any{
				{"type": "stdio", "name": "mcp-demo"},
			},
		},
	}))

	if len(out) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(out), out)
	}

	var payload map[string]any
	mustUnmarshalLine(t, out[0], &payload)
	if _, ok := payload["error"]; ok {
		t.Fatalf("session/load returned error: %v", payload["error"])
	}
	if _, ok := payload["result"].(map[string]any); !ok {
		t.Fatalf("session/load result should be object: %v", payload)
	}
}

func TestPromptAndLoadSessionConcurrent(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")
	sessionID := createSession(t, srv)

	for i := 0; i < 200; i++ {
		out := runServe(t, srv,
			line(map[string]any{
				"jsonrpc": "2.0",
				"id":      1000 + i*2,
				"method":  "session/prompt",
				"params": map[string]any{
					"sessionId": sessionID,
					"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
				},
			}),
			line(map[string]any{
				"jsonrpc": "2.0",
				"id":      1001 + i*2,
				"method":  "session/load",
				"params": map[string]any{
					"sessionId": sessionID,
					"cwd":       "/tmp/loaded",
					"mcpServers": []map[string]any{
						{"type": "stdio", "name": "mcp-demo"},
					},
				},
			}),
		)

		var promptOK bool
		var loadOK bool
		for _, raw := range out {
			var payload map[string]any
			mustUnmarshalLine(t, raw, &payload)

			if payload["error"] != nil {
				t.Fatalf("unexpected error payload: %v", payload)
			}

			id, ok := payload["id"].(float64)
			if !ok {
				continue
			}
			switch int(id) {
			case 1000 + i*2:
				result := payload["result"].(map[string]any)
				if result["stopReason"] == "end_turn" {
					promptOK = true
				}
			case 1001 + i*2:
				if _, ok := payload["result"].(map[string]any); ok {
					loadOK = true
				}
			}
		}

		if !promptOK || !loadOK {
			t.Fatalf("expected both prompt and load responses, got output: %v", out)
		}
	}
}

func TestListSessions(t *testing.T) {
	srv := NewServer(&fakeExecutor{reply: "ok"}, nil, "test")
	sessionA := createSession(t, srv)

	_ = runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      12,
		"method":  "session/new",
		"params": map[string]any{
			"cwd": "/var/tmp",
		},
	}))

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "session/list",
		"params":  map[string]any{},
	}))

	if len(out) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(out), out)
	}

	var payload map[string]any
	mustUnmarshalLine(t, out[0], &payload)
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("session/list missing result object: %v", payload)
	}

	sessions, ok := result["sessions"].([]any)
	if !ok || len(sessions) < 2 {
		t.Fatalf("expected at least 2 sessions, got: %v", result["sessions"])
	}

	var foundA bool
	for _, raw := range sessions {
		item := raw.(map[string]any)
		id, _ := item["sessionId"].(string)
		cwd, _ := item["cwd"].(string)
		if strings.TrimSpace(id) == "" || strings.TrimSpace(cwd) == "" {
			t.Fatalf("session/list returned invalid item: %v", item)
		}
		if _, ok := item["updatedAt"].(string); !ok {
			t.Fatalf("session/list item missing updatedAt: %v", item)
		}
		if id == sessionA {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("session/list missing known session %s: %v", sessionA, result["sessions"])
	}
}

func createSession(t *testing.T, srv *Server) string {
	t.Helper()

	out := runServe(t, srv, line(map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "session/new",
		"params": map[string]any{
			"cwd": "/tmp",
		},
	}))

	if len(out) != 1 {
		t.Fatalf("expected 1 output line, got %d: %v", len(out), out)
	}

	var payload map[string]any
	mustUnmarshalLine(t, out[0], &payload)
	result := payload["result"].(map[string]any)
	return result["sessionId"].(string)
}

func runServe(t *testing.T, srv *Server, lines ...string) []string {
	t.Helper()

	input := strings.Join(lines, "\n") + "\n"
	var out bytes.Buffer

	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	raw := strings.TrimSpace(out.String())
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func line(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustUnmarshalLine(t *testing.T, l string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(l), out); err != nil {
		t.Fatalf("failed to unmarshal line %q: %v", l, err)
	}
}
