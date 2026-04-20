package acpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	methodFSWriteTextFile     = "fs/write_text_file"
	methodFSReadTextFile      = "fs/read_text_file"
	methodTerminalCreate      = "terminal/create"
	methodTerminalOutput      = "terminal/output"
	methodTerminalWaitForExit = "terminal/wait_for_exit"
	methodTerminalKill        = "terminal/kill"
	methodTerminalRelease     = "terminal/release"
)

const (
	toolFSReadTextFile     = "fs_read_text_file"
	toolFSWriteTextFile    = "fs_write_text_file"
	toolTerminalCreate     = "terminal_create"
	toolTerminalOutput     = "terminal_output"
	toolTerminalWaitExit   = "terminal_wait_for_exit"
	toolTerminalKill       = "terminal_kill"
	toolTerminalRelease    = "terminal_release"
	toolCallStatusComplete = "completed"
	toolCallStatusFailed   = "failed"
	toolCallStatusProgress = "in_progress"
)

type RuntimeToolInvoker interface {
	ToolInfos(ctx context.Context) ([]RuntimeToolInfo, error)
	InvokeTool(ctx context.Context, toolCallID, toolName, argumentsInJSON string) (string, error)
}

type RuntimeToolInfo struct {
	Name   string
	Desc   string
	Params map[string]*ToolParamInfo
}

type ToolParamType string

const (
	ToolParamObject  ToolParamType = "object"
	ToolParamNumber  ToolParamType = "number"
	ToolParamInteger ToolParamType = "integer"
	ToolParamString  ToolParamType = "string"
	ToolParamArray   ToolParamType = "array"
	ToolParamNull    ToolParamType = "null"
	ToolParamBoolean ToolParamType = "boolean"
)

type ToolParamInfo struct {
	Type      ToolParamType
	ElemInfo  *ToolParamInfo
	SubParams map[string]*ToolParamInfo
	Desc      string
	Enum      []string
	Required  bool
}

type runtimeToolInvoker struct {
	server    *Server
	sessionID string
	cwd       string
	caps      clientCapabilities
}

func (s *Server) newRuntimeToolInvoker(sessionID, cwd string) RuntimeToolInvoker {
	return &runtimeToolInvoker{
		server:    s,
		sessionID: sessionID,
		cwd:       cwd,
		caps:      s.getClientCapabilities(),
	}
}

func (i *runtimeToolInvoker) ToolInfos(_ context.Context) ([]RuntimeToolInfo, error) {
	tools := make([]RuntimeToolInfo, 0, 7)

	if i.caps.FS.ReadTextFile {
		tools = append(tools, RuntimeToolInfo{
			Name: toolFSReadTextFile,
			Desc: "Read a text file from the ACP client using fs/read_text_file.",
			Params: map[string]*ToolParamInfo{
				"path": {Type: ToolParamString, Required: true, Desc: "Absolute or session-relative file path."},
				"line": {Type: ToolParamInteger, Required: false, Desc: "1-based start line."},
				"limit": {
					Type:     ToolParamInteger,
					Required: false,
					Desc:     "Maximum number of lines to read.",
				},
			},
		})
	}

	if i.caps.FS.WriteTextFile {
		tools = append(tools, RuntimeToolInfo{
			Name: toolFSWriteTextFile,
			Desc: "Write a text file via ACP fs/write_text_file.",
			Params: map[string]*ToolParamInfo{
				"path":    {Type: ToolParamString, Required: true, Desc: "Absolute or session-relative file path."},
				"content": {Type: ToolParamString, Required: true, Desc: "Full text content to write."},
			},
		})
	}

	if i.caps.Terminal {
		tools = append(tools,
			RuntimeToolInfo{
				Name: toolTerminalCreate,
				Desc: "Create a terminal command via ACP terminal/create.",
				Params: map[string]*ToolParamInfo{
					"command": {Type: ToolParamString, Required: true, Desc: "Executable to run."},
					"args": {Type: ToolParamArray, Required: false, Desc: "Command arguments.", ElemInfo: &ToolParamInfo{
						Type: ToolParamString,
					}},
					"cwd": {Type: ToolParamString, Required: false, Desc: "Optional absolute or session-relative working directory."},
				},
			},
			RuntimeToolInfo{
				Name: toolTerminalOutput,
				Desc: "Get current output via ACP terminal/output.",
				Params: map[string]*ToolParamInfo{
					"terminalId": {Type: ToolParamString, Required: true, Desc: "Terminal ID from terminal_create."},
				},
			},
			RuntimeToolInfo{
				Name: toolTerminalWaitExit,
				Desc: "Wait for terminal command exit via ACP terminal/wait_for_exit.",
				Params: map[string]*ToolParamInfo{
					"terminalId": {Type: ToolParamString, Required: true, Desc: "Terminal ID from terminal_create."},
				},
			},
			RuntimeToolInfo{
				Name: toolTerminalKill,
				Desc: "Kill a running terminal command via ACP terminal/kill.",
				Params: map[string]*ToolParamInfo{
					"terminalId": {Type: ToolParamString, Required: true, Desc: "Terminal ID from terminal_create."},
				},
			},
			RuntimeToolInfo{
				Name: toolTerminalRelease,
				Desc: "Release a terminal resource via ACP terminal/release.",
				Params: map[string]*ToolParamInfo{
					"terminalId": {Type: ToolParamString, Required: true, Desc: "Terminal ID from terminal_create."},
				},
			},
		)
	}

	return tools, nil
}

func (i *runtimeToolInvoker) InvokeTool(ctx context.Context, toolCallID, toolName, argumentsInJSON string) (string, error) {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		toolCallID = newSessionID()
	}

	rawInput := decodeJSONValue(argumentsInJSON)
	i.server.logf("[tool] start call_id=%s name=%s args=%s", toolCallID, toolName, argumentsInJSON)

	i.server.writeSessionUpdateRaw(i.sessionID, map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    toolCallID,
		"title":         toolName,
		"status":        toolCallStatusProgress,
		"rawInput":      rawInput,
	})

	result, err := i.invokeTool(ctx, toolName, argumentsInJSON)
	if err != nil {
		i.server.writeSessionUpdateRaw(i.sessionID, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    toolCallID,
			"status":        toolCallStatusFailed,
			"content":       textToolCallContent(err.Error()),
		})
		i.server.logf("[tool] failed call_id=%s name=%s err=%v", toolCallID, toolName, err)
		return "", err
	}

	i.server.writeSessionUpdateRaw(i.sessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    toolCallID,
		"status":        toolCallStatusComplete,
		"rawOutput":     decodeJSONValue(result),
		"content":       textToolCallContent(result),
	})
	i.server.logf("[tool] done call_id=%s name=%s result=%s", toolCallID, toolName, previewText(result, 200))
	return result, nil
}

func (i *runtimeToolInvoker) invokeTool(ctx context.Context, toolName, argumentsInJSON string) (string, error) {
	switch toolName {
	case toolFSReadTextFile:
		if !i.caps.FS.ReadTextFile {
			return "", fmt.Errorf("tool %s is not available: fs.readTextFile not supported by client", toolName)
		}
		var args readTextFileToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("path is required")
		}

		var out readTextFileResponse
		if err := i.server.callClient(ctx, methodFSReadTextFile, readTextFileRequest{
			SessionID: i.sessionID,
			Path:      i.resolvePath(args.Path),
			Line:      args.Line,
			Limit:     args.Limit,
		}, &out); err != nil {
			return "", err
		}
		return out.Content, nil

	case toolFSWriteTextFile:
		if !i.caps.FS.WriteTextFile {
			return "", fmt.Errorf("tool %s is not available: fs.writeTextFile not supported by client", toolName)
		}
		var args writeTextFileToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("path is required")
		}

		if err := i.server.callClient(ctx, methodFSWriteTextFile, writeTextFileRequest{
			SessionID: i.sessionID,
			Path:      i.resolvePath(args.Path),
			Content:   args.Content,
		}, nil); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil

	case toolTerminalCreate:
		if !i.caps.Terminal {
			return "", fmt.Errorf("tool %s is not available: terminal not supported by client", toolName)
		}
		var args terminalCreateToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.Command) == "" {
			return "", fmt.Errorf("command is required")
		}

		var reqCWD *string
		if strings.TrimSpace(args.CWD) != "" {
			cwd := i.resolvePath(args.CWD)
			reqCWD = &cwd
		}

		var out createTerminalResponse
		if err := i.server.callClient(ctx, methodTerminalCreate, createTerminalRequest{
			SessionID: i.sessionID,
			Command:   args.Command,
			Args:      args.Args,
			CWD:       reqCWD,
		}, &out); err != nil {
			return "", err
		}

		result := map[string]any{"terminalId": out.TerminalID}
		b, _ := json.Marshal(result)
		return string(b), nil

	case toolTerminalOutput:
		if !i.caps.Terminal {
			return "", fmt.Errorf("tool %s is not available: terminal not supported by client", toolName)
		}
		var args terminalIDToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.TerminalID) == "" {
			return "", fmt.Errorf("terminalId is required")
		}

		var out terminalOutputResponse
		if err := i.server.callClient(ctx, methodTerminalOutput, terminalIDRequest{
			SessionID:  i.sessionID,
			TerminalID: args.TerminalID,
		}, &out); err != nil {
			return "", err
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case toolTerminalWaitExit:
		if !i.caps.Terminal {
			return "", fmt.Errorf("tool %s is not available: terminal not supported by client", toolName)
		}
		var args terminalIDToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.TerminalID) == "" {
			return "", fmt.Errorf("terminalId is required")
		}

		var out waitForExitResponse
		if err := i.server.callClient(ctx, methodTerminalWaitForExit, terminalIDRequest{
			SessionID:  i.sessionID,
			TerminalID: args.TerminalID,
		}, &out); err != nil {
			return "", err
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case toolTerminalKill:
		if !i.caps.Terminal {
			return "", fmt.Errorf("tool %s is not available: terminal not supported by client", toolName)
		}
		var args terminalIDToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.TerminalID) == "" {
			return "", fmt.Errorf("terminalId is required")
		}

		if err := i.server.callClient(ctx, methodTerminalKill, terminalIDRequest{
			SessionID:  i.sessionID,
			TerminalID: args.TerminalID,
		}, nil); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil

	case toolTerminalRelease:
		if !i.caps.Terminal {
			return "", fmt.Errorf("tool %s is not available: terminal not supported by client", toolName)
		}
		var args terminalIDToolArgs
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid args for %s: %w", toolName, err)
		}
		if strings.TrimSpace(args.TerminalID) == "" {
			return "", fmt.Errorf("terminalId is required")
		}

		if err := i.server.callClient(ctx, methodTerminalRelease, terminalIDRequest{
			SessionID:  i.sessionID,
			TerminalID: args.TerminalID,
		}, nil); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	}

	return "", fmt.Errorf("unknown tool: %s", toolName)
}

func (i *runtimeToolInvoker) resolvePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return trimmed
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed)
	}
	return filepath.Clean(filepath.Join(i.cwd, trimmed))
}

func decodeJSONValue(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return v
}

func textToolCallContent(text string) []map[string]any {
	return []map[string]any{
		{
			"type": "content",
			"content": map[string]any{
				"type": "text",
				"text": text,
			},
		},
	}
}

func previewText(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

type readTextFileToolArgs struct {
	Path  string `json:"path"`
	Line  *int   `json:"line,omitempty"`
	Limit *int   `json:"limit,omitempty"`
}

type writeTextFileToolArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type terminalCreateToolArgs struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	CWD     string   `json:"cwd,omitempty"`
}

type terminalIDToolArgs struct {
	TerminalID string `json:"terminalId"`
}

type readTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

type readTextFileResponse struct {
	Content string `json:"content"`
}

type writeTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type createTerminalRequest struct {
	SessionID string   `json:"sessionId"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	CWD       *string  `json:"cwd,omitempty"`
}

type createTerminalResponse struct {
	TerminalID string `json:"terminalId"`
}

type terminalIDRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type terminalOutputResponse struct {
	Output     string               `json:"output"`
	Truncated  bool                 `json:"truncated"`
	ExitStatus *waitForExitResponse `json:"exitStatus,omitempty"`
}

type waitForExitResponse struct {
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
}
