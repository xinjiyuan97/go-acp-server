package acpserver

import "context"

// SessionMCPServer describes one MCP server bound to the current session.
type SessionMCPServer struct {
	Type    string
	Name    string
	Command string
	Args    []string
	URL     string
}

// SessionRuntimeContext carries runtime session metadata for one prompt turn.
//
// Use SessionRuntimeContextFromContext or SessionIDFromContext inside your
// PromptExecutor implementation to access these values.
type SessionRuntimeContext struct {
	SessionID  string
	CWD        string
	MCPServers []SessionMCPServer
}

type sessionRuntimeContextKey struct{}

func withSessionRuntimeContext(ctx context.Context, sessionID string, snapshot sessionSnapshot) context.Context {
	meta := SessionRuntimeContext{
		SessionID:  sessionID,
		CWD:        snapshot.cwd,
		MCPServers: make([]SessionMCPServer, 0, len(snapshot.mcpServers)),
	}
	for _, m := range snapshot.mcpServers {
		meta.MCPServers = append(meta.MCPServers, SessionMCPServer{
			Type:    m.Type,
			Name:    m.Name,
			Command: m.Command,
			Args:    append([]string(nil), m.Args...),
			URL:     m.URL,
		})
	}
	return context.WithValue(ctx, sessionRuntimeContextKey{}, meta)
}

// SessionRuntimeContextFromContext returns prompt-scoped session metadata.
func SessionRuntimeContextFromContext(ctx context.Context) (SessionRuntimeContext, bool) {
	if ctx == nil {
		return SessionRuntimeContext{}, false
	}
	meta, ok := ctx.Value(sessionRuntimeContextKey{}).(SessionRuntimeContext)
	if !ok || meta.SessionID == "" {
		return SessionRuntimeContext{}, false
	}
	return meta, true
}

// SessionIDFromContext returns the current prompt session id.
func SessionIDFromContext(ctx context.Context) (string, bool) {
	meta, ok := SessionRuntimeContextFromContext(ctx)
	if !ok {
		return "", false
	}
	return meta.SessionID, true
}
