package acpserver

import (
	"bytes"
	"encoding/json"
)

type incomingMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m incomingMessage) hasID() bool {
	return len(bytes.TrimSpace(m.ID)) > 0
}

type outgoingSuccess struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type outgoingRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

type outgoingError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcError        `json:"error"`
}

type outgoingNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeRequest struct {
	ProtocolVersion    any                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities,omitempty"`
}

type initializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []any             `json:"authMethods"`
	AgentInfo         *implementation   `json:"agentInfo,omitempty"`
}

type implementation struct {
	Name    string  `json:"name"`
	Title   *string `json:"title,omitempty"`
	Version string  `json:"version"`
}

type agentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	SessionCapabilities sessionCapabilities `json:"sessionCapabilities,omitempty"`
}

type sessionCapabilities struct {
	List *emptyCapabilities `json:"list,omitempty"`
}

type emptyCapabilities struct{}

type authenticateRequest struct {
	MethodID string `json:"methodId"`
}

type authenticateResponse struct{}

type newSessionRequest struct {
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers,omitempty"`
}

type newSessionResponse struct {
	SessionID string `json:"sessionId"`
}

type loadSessionRequest struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers,omitempty"`
}

type loadSessionResponse struct{}

type listSessionsRequest struct {
	CWD    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type listSessionsResponse struct {
	Sessions   []sessionInfo `json:"sessions"`
	NextCursor *string       `json:"nextCursor,omitempty"`
}

type sessionInfo struct {
	SessionID string  `json:"sessionId"`
	CWD       string  `json:"cwd"`
	Title     *string `json:"title,omitempty"`
	UpdatedAt *string `json:"updatedAt,omitempty"`
}

type promptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type promptResponse struct {
	StopReason string `json:"stopReason"`
}

type cancelNotification struct {
	SessionID string `json:"sessionId"`
}

type sessionNotification struct {
	SessionID string        `json:"sessionId"`
	Update    sessionUpdate `json:"update"`
}

type sessionUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

// ContentBlock is one ACP content block (e.g. text/image/audio/resource).
//
// It intentionally keeps raw key/value pairs so the server can pass through
// evolving ACP block shapes without dropping fields.
type ContentBlock map[string]any

type clientCapabilities struct {
	FS       fileSystemCapabilities `json:"fs,omitempty"`
	Terminal bool                   `json:"terminal,omitempty"`
}

type fileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

type mcpServer struct {
	Type    string   `json:"type,omitempty"`
	Name    string   `json:"name,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
}
