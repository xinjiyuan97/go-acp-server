# go-acp-server

A standalone ACP (Agent Client Protocol) server module in Go.

This module focuses on:

- ACP JSON-RPC request/response handling over stdio
- Session lifecycle (`initialize`, `session/new`, `session/load`, `session/list`, `session/prompt`, `session/cancel`)
- Streaming updates (`session/update`: `user_message_chunk`, `agent_message_chunk`, `agent_thought_chunk`, `plan`, `available_commands_update`, `current_mode_update`, `config_option_update`, `tool_call`, `tool_call_update`, `session_info_update`)
- Agent -> client tool RPC bridge (`fs/*`, `terminal/*`)
- Tool-call tracing logs for runtime verification

It is intentionally model-agnostic: you provide a `PromptExecutor`.

## Package

```go
import acpserver "github.com/xinjiyuan97/go-acp-server"
```

## Quick Start

Implement `PromptExecutor`:

```go
type MyExecutor struct{}

func (e *MyExecutor) StreamReply(
    ctx context.Context,
    prompt string,
    tools acpserver.RuntimeToolInvoker,
    onChunk func(string) error,
) (string, error) {
    // optional: read prompt-scoped session metadata from context
    if sid, ok := acpserver.SessionIDFromContext(ctx); ok {
        _ = sid
    }

    // 1) optional: call tools.InvokeTool(...) if your model/tool loop decides to use tools
    // 2) stream chunks through onChunk
    // 3) return final assistant text
    reply := "hello from executor"
    if onChunk != nil {
        _ = onChunk(reply)
    }
    return reply, nil
}
```

If you want to stream richer ACP updates (thought/plan/mode/config/commands), implement `PromptExecutorWithUpdates` and emit through `PromptUpdateWriter`.

By default, server-side prompt flow already emits dynamic context updates before model generation:

- `current_mode_update` (default mode)
- `available_commands_update` (derived from runtime tools/capabilities)
- `config_option_update` (derived from session cwd/mcp bindings and client capabilities)

Start server:

```go
srv := acpserver.NewServer(&MyExecutor{}, os.Stderr, "v0.1.0")
if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
    panic(err)
}
```

## Runtime Tools

`RuntimeToolInvoker` provides two key methods:

- `ToolInfos(ctx)` returns tool metadata that your model can bind.
- `InvokeTool(ctx, toolCallID, toolName, argumentsInJSON)` sends ACP client RPC calls and returns tool output.

Supported ACP client methods:

- `fs/read_text_file`
- `fs/write_text_file`
- `terminal/create`
- `terminal/output`
- `terminal/wait_for_exit`
- `terminal/kill`
- `terminal/release`

Tool exposure is capability-driven (from `initialize.clientCapabilities`).

## Connections Extensions

This module now includes a `connections` package to extend ACP transport usage
beyond stdio.

```go
import (
    acpserver "github.com/xinjiyuan97/go-acp-server"
    "github.com/xinjiyuan97/go-acp-server/connections"
)
```

Create a shared endpoint:

```go
srv := acpserver.NewServer(&MyExecutor{}, os.Stderr, "v0.1.0")
endpoint := connections.NewEndpoint(srv)
```

HTTP extension (NDJSON over POST):

```go
handler := connections.NewHTTPHandler(endpoint, connections.HTTPHandlerOptions{
    MaxBodyBytes: 4 * 1024 * 1024,
})
http.ListenAndServe(":8080", handler)
```

WebSocket extension (library-agnostic adapter):

```go
type MyWSConn struct{}

func (c *MyWSConn) ReadText(ctx context.Context) (string, error)   { /* ... */ }
func (c *MyWSConn) WriteText(ctx context.Context, text string) error { /* ... */ }
func (c *MyWSConn) Close() error { /* ... */ }

_ = connections.ServeWebSocket(context.Background(), endpoint, &MyWSConn{})
```

## Logging

Pass a logger (e.g. `os.Stderr`) to `NewServer` to inspect runtime behavior:

- `[tool] start ...`
- `[acp->client] method=...`
- `[client->acp] result=...`
- `[tool] done ...` / `[tool] failed ...`

These logs are useful to verify tools are actually invoked instead of only being mentioned in prompt text.

## Testing

```bash
cd crates/go-acp-server
go test ./...
```

## Chinese Guide

See:

- `docs/使用说明.md`
