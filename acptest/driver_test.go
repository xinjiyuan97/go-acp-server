package acptest_test

import (
	"context"
	"io"
	"strings"
	"testing"

	acpserver "github.com/xinjiyuan97/go-acp-server"
	"github.com/xinjiyuan97/go-acp-server/acptest"
)

type echoExecutor struct{}

func (e *echoExecutor) StreamReply(
	_ context.Context,
	prompt []acpserver.ContentBlock,
	_ acpserver.RuntimeToolInvoker,
	_ acpserver.PromptUpdateWriter,
) (string, error) {
	text := ""
	for _, block := range prompt {
		typ, _ := block["type"].(string)
		if typ != "text" {
			continue
		}
		text, _ = block["text"].(string)
		if strings.TrimSpace(text) != "" {
			break
		}
	}
	return "echo:" + text, nil
}

type toolExecutor struct{}

func (e *toolExecutor) StreamReply(
	ctx context.Context,
	_ []acpserver.ContentBlock,
	tools acpserver.RuntimeToolInvoker,
	_ acpserver.PromptUpdateWriter,
) (string, error) {
	content, err := tools.InvokeTool(ctx, "read-1", "fs_read_text_file", `{"path":"a.txt"}`)
	if err != nil {
		return "", err
	}
	return "content=" + content, nil
}

func TestDriverSupportsMultiTurnAndFormatting(t *testing.T) {
	srv := acpserver.NewServer(&echoExecutor{}, io.Discard, "test")
	d := acptest.NewDriver(srv)
	defer func() {
		if err := d.Close(); err != nil {
			t.Fatalf("close driver failed: %v", err)
		}
	}()

	if _, err := d.Initialize(1, map[string]any{}); err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
	sessionID, _, err := d.NewSession("/tmp", nil)
	if err != nil {
		t.Fatalf("new session failed: %v", err)
	}
	if strings.TrimSpace(sessionID) == "" {
		t.Fatalf("session id should not be empty")
	}

	turn1, err := d.PromptTextCurrent("hello")
	if err != nil {
		t.Fatalf("prompt 1 failed: %v", err)
	}
	turn2, err := d.PromptTextCurrent("again")
	if err != nil {
		t.Fatalf("prompt 2 failed: %v", err)
	}

	if got := turn1.AssistantText; got != "echo:hello" {
		t.Fatalf("unexpected turn1 assistant text: %q", got)
	}
	if got := turn2.AssistantText; got != "echo:again" {
		t.Fatalf("unexpected turn2 assistant text: %q", got)
	}
	if got := turn2.StopReason; got != "end_turn" {
		t.Fatalf("unexpected turn2 stopReason: %q", got)
	}

	formatted := turn2.Format()
	for _, expected := range []string{"Session:", "StopReason:", "Assistant:"} {
		if !strings.Contains(formatted, expected) {
			t.Fatalf("formatted output missing %q: %s", expected, formatted)
		}
	}
}

func TestDriverCanStubClientRPCForToolCalls(t *testing.T) {
	srv := acpserver.NewServer(&toolExecutor{}, io.Discard, "test")
	d := acptest.NewDriver(srv)
	defer func() {
		if err := d.Close(); err != nil {
			t.Fatalf("close driver failed: %v", err)
		}
	}()

	if _, err := d.Initialize(1, map[string]any{
		"fs": map[string]any{"readTextFile": true},
	}); err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
	if _, _, err := d.NewSession("/tmp", nil); err != nil {
		t.Fatalf("new session failed: %v", err)
	}

	d.QueueClientResult("fs/read_text_file", map[string]any{"content": "from-client"})

	turn, err := d.PromptTextCurrent("please read")
	if err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if got := turn.AssistantText; got != "content=from-client" {
		t.Fatalf("unexpected assistant text: %q", got)
	}

	var seenStart, seenDone bool
	for _, ev := range turn.ToolEvents {
		if ev.UpdateType == "tool_call" && ev.ToolCallID == "read-1" {
			seenStart = true
		}
		if ev.UpdateType == "tool_call_update" && ev.ToolCallID == "read-1" && ev.Status == "completed" {
			seenDone = true
		}
	}
	if !seenStart || !seenDone {
		t.Fatalf("expected tool progress events, got %+v", turn.ToolEvents)
	}
}
