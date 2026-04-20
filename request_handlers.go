package acpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Server) handleRequest(ctx context.Context, req incomingMessage) {
	switch req.Method {
	case methodInitialize:
		s.handleInitialize(req)
	case methodAuthenticate:
		s.handleAuthenticate(req)
	case methodSessionNew:
		s.handleNewSession(req)
	case methodSessionLoad:
		s.handleLoadSession(req)
	case methodSessionList:
		s.handleListSessions(req)
	case methodSessionPrompt:
		s.handlePrompt(ctx, req)
	default:
		s.writeError(req.ID, errMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleNotification(req incomingMessage) {
	switch req.Method {
	case methodSessionCancel:
		var args cancelNotification
		if err := decodeParams(req.Params, &args); err != nil {
			s.logf("invalid %s params: %v", methodSessionCancel, err)
			return
		}
		s.cancelSession(args.SessionID)
	default:
		// Ignore unknown notifications for forward compatibility.
	}
}

func (s *Server) handleInitialize(req incomingMessage) {
	var args initializeRequest
	if err := decodeParams(req.Params, &args); err != nil {
		s.writeError(req.ID, errInvalidParams, err.Error())
		return
	}

	s.clientCapsMu.Lock()
	s.clientCaps = args.ClientCapabilities
	s.clientCapsMu.Unlock()

	res := initializeResponse{
		ProtocolVersion: protocolVersionV1,
		AgentCapabilities: agentCapabilities{
			LoadSession: true,
			SessionCapabilities: sessionCapabilities{
				List: &emptyCapabilities{},
			},
		},
		AuthMethods: []any{},
		AgentInfo: &implementation{
			Name:    "eino-acp-agent",
			Title:   strPtr("Eino ACP Agent"),
			Version: s.buildVersion(),
		},
	}

	s.writeResult(req.ID, res)
}

func (s *Server) handleAuthenticate(req incomingMessage) {
	var args authenticateRequest
	if err := decodeParams(req.Params, &args); err != nil {
		s.writeError(req.ID, errInvalidParams, err.Error())
		return
	}

	// This minimal server uses no auth methods but stays protocol-compatible.
	s.writeResult(req.ID, authenticateResponse{})
}

func (s *Server) handleNewSession(req incomingMessage) {
	var args newSessionRequest
	if err := decodeParams(req.Params, &args); err != nil {
		s.writeError(req.ID, errInvalidParams, err.Error())
		return
	}

	if args.CWD == "" {
		s.writeError(req.ID, errInvalidParams, "cwd is required")
		return
	}

	if !filepath.IsAbs(args.CWD) {
		s.writeError(req.ID, errInvalidParams, "cwd must be an absolute path")
		return
	}

	sessionID := newSessionID()

	s.sessionsMu.Lock()
	s.sessions[sessionID] = &sessionState{
		CWD:        args.CWD,
		mcpServers: append([]mcpServer(nil), args.MCPServers...),
		updatedAt:  time.Now().UTC(),
	}
	s.sessionsMu.Unlock()

	s.clearCancelFlag(sessionID)

	s.writeResult(req.ID, newSessionResponse{SessionID: sessionID})
}

func (s *Server) handleLoadSession(req incomingMessage) {
	var args loadSessionRequest
	if err := decodeParams(req.Params, &args); err != nil {
		s.writeError(req.ID, errInvalidParams, err.Error())
		return
	}

	sessionID := strings.TrimSpace(args.SessionID)
	if sessionID == "" {
		s.writeError(req.ID, errInvalidParams, "sessionId is required")
		return
	}
	if strings.TrimSpace(args.CWD) == "" {
		s.writeError(req.ID, errInvalidParams, "cwd is required")
		return
	}
	if !filepath.IsAbs(args.CWD) {
		s.writeError(req.ID, errInvalidParams, "cwd must be an absolute path")
		return
	}

	session := s.getSession(sessionID)
	if session == nil {
		s.writeError(req.ID, errInvalidParams, fmt.Sprintf("unknown session '%s'", sessionID))
		return
	}

	session.mu.Lock()
	session.CWD = args.CWD
	session.mcpServers = append([]mcpServer(nil), args.MCPServers...)
	session.updatedAt = time.Now().UTC()
	session.mu.Unlock()

	s.clearCancelFlag(sessionID)
	s.writeResult(req.ID, loadSessionResponse{})
}

func (s *Server) handleListSessions(req incomingMessage) {
	var args listSessionsRequest
	if err := decodeParams(req.Params, &args); err != nil {
		s.writeError(req.ID, errInvalidParams, err.Error())
		return
	}

	cwdFilter := strings.TrimSpace(args.CWD)
	if cwdFilter != "" && !filepath.IsAbs(cwdFilter) {
		s.writeError(req.ID, errInvalidParams, "cwd must be an absolute path")
		return
	}

	cursor := strings.TrimSpace(args.Cursor)
	if cursor != "" {
		// Cursor pagination is intentionally not implemented in this minimal server.
		s.writeError(req.ID, errInvalidParams, "cursor is not supported")
		return
	}

	type listedSession struct {
		id      string
		cwd     string
		title   string
		updated string
	}

	s.sessionsMu.RLock()
	snapshots := make([]listedSession, 0, len(s.sessions))
	for id, session := range s.sessions {
		session.mu.Lock()
		item := listedSession{
			id:      id,
			cwd:     session.CWD,
			title:   strings.TrimSpace(session.Title),
			updated: formatRFC3339UTC(session.updatedAt),
		}
		session.mu.Unlock()
		snapshots = append(snapshots, item)
	}
	s.sessionsMu.RUnlock()

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].id < snapshots[j].id
	})

	out := make([]sessionInfo, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if cwdFilter != "" && snapshot.cwd != cwdFilter {
			continue
		}
		row := sessionInfo{
			SessionID: snapshot.id,
			CWD:       snapshot.cwd,
		}
		if snapshot.title != "" {
			title := snapshot.title
			row.Title = &title
		}
		if snapshot.updated != "" {
			updated := snapshot.updated
			row.UpdatedAt = &updated
		}
		out = append(out, row)
	}

	s.writeResult(req.ID, listSessionsResponse{Sessions: out})
}

func (s *Server) handlePrompt(ctx context.Context, req incomingMessage) {
	var args promptRequest
	if err := decodeParams(req.Params, &args); err != nil {
		s.writeError(req.ID, errInvalidParams, err.Error())
		return
	}

	session := s.getSession(args.SessionID)
	if session == nil {
		s.writeError(req.ID, errInvalidParams, fmt.Sprintf("unknown session '%s'", args.SessionID))
		return
	}

	if len(args.Prompt) == 0 {
		s.writeError(req.ID, errInvalidParams, "prompt must include at least one content block")
		return
	}
	userPromptText := extractPromptText(args.Prompt)

	if s.consumeCancelFlag(args.SessionID) {
		s.writeResult(req.ID, promptResponse{StopReason: stopReasonCancelled})
		return
	}

	promptCtx, cancel := context.WithCancel(ctx)
	s.setActiveCancel(args.SessionID, cancel)
	defer s.clearActiveCancel(args.SessionID)

	var chunkEmitted bool
	var sessionUpdatedAt string
	var sessionTitle string

	updates := newPromptUpdateWriter(s, args.SessionID, promptCtx, func() {
		chunkEmitted = true
	})
	if strings.TrimSpace(userPromptText) != "" {
		if err := updates.UserMessageChunk(userPromptText); err != nil {
			if s.consumeCancelFlag(args.SessionID) || promptCtx.Err() == context.Canceled {
				s.writeResult(req.ID, promptResponse{StopReason: stopReasonCancelled})
				return
			}
			s.writeError(req.ID, errInternalError, fmt.Sprintf("failed to emit user_message_chunk: %v", err))
			return
		}
	}

	session.mu.Lock()
	snapshot := sessionSnapshot{
		cwd:        session.CWD,
		mcpServers: append([]mcpServer(nil), session.mcpServers...),
	}
	toolInvoker := s.newRuntimeToolInvoker(args.SessionID, snapshot.cwd)
	session.mu.Unlock()
	promptCtx = withSessionRuntimeContext(promptCtx, args.SessionID, snapshot)

	promptBlocks := cloneContentBlocks(args.Prompt)

	if err := s.emitSessionContextUpdates(promptCtx, updates, snapshot, toolInvoker); err != nil {
		if s.consumeCancelFlag(args.SessionID) || promptCtx.Err() == context.Canceled {
			s.writeResult(req.ID, promptResponse{StopReason: stopReasonCancelled})
			return
		}
		s.writeError(req.ID, errInternalError, fmt.Sprintf("failed to emit session context updates: %v", err))
		return
	}

	var reply string
	var err error
	reply, err = s.executor.StreamReply(promptCtx, promptBlocks, toolInvoker, updates)
	if err == nil {
		session.mu.Lock()
		session.updatedAt = time.Now().UTC()
		sessionUpdatedAt = formatRFC3339UTC(session.updatedAt)
		sessionTitle = strings.TrimSpace(session.Title)
		session.mu.Unlock()
	}

	if err != nil {
		if s.consumeCancelFlag(args.SessionID) || promptCtx.Err() == context.Canceled {
			s.writeResult(req.ID, promptResponse{StopReason: stopReasonCancelled})
			return
		}
		s.writeError(req.ID, errInternalError, fmt.Sprintf("prompt failed: %v", err))
		return
	}

	if strings.TrimSpace(reply) != "" && !chunkEmitted {
		if err := updates.AgentMessageChunk(reply); err != nil {
			if s.consumeCancelFlag(args.SessionID) || promptCtx.Err() == context.Canceled {
				s.writeResult(req.ID, promptResponse{StopReason: stopReasonCancelled})
				return
			}
			s.writeError(req.ID, errInternalError, fmt.Sprintf("failed to emit agent_message_chunk: %v", err))
			return
		}
	}

	if s.consumeCancelFlag(args.SessionID) {
		s.writeResult(req.ID, promptResponse{StopReason: stopReasonCancelled})
		return
	}

	s.writeSessionInfoUpdate(args.SessionID, sessionTitle, sessionUpdatedAt)
	s.writeResult(req.ID, promptResponse{StopReason: stopReasonEndTurn})
}

func (s *Server) emitSessionContextUpdates(
	ctx context.Context,
	updates PromptUpdateWriter,
	snapshot sessionSnapshot,
	tools RuntimeToolInvoker,
) error {
	if err := updates.CurrentMode("default"); err != nil {
		return err
	}

	toolInfos, err := tools.ToolInfos(ctx)
	if err != nil {
		return err
	}
	sort.Slice(toolInfos, func(i, j int) bool {
		return toolInfos[i].Name < toolInfos[j].Name
	})

	commands := make([]AvailableCommandUpdate, 0, len(toolInfos))
	for _, info := range toolInfos {
		name := strings.TrimSpace(info.Name)
		if name == "" {
			continue
		}
		description := strings.TrimSpace(info.Desc)
		if description == "" {
			description = "ACP runtime tool"
		}
		commands = append(commands, AvailableCommandUpdate{
			Name:        name,
			Description: description,
		})
	}
	if err := updates.AvailableCommands(commands); err != nil {
		return err
	}

	mcpNames := make([]string, 0, len(snapshot.mcpServers))
	for _, m := range snapshot.mcpServers {
		name := strings.TrimSpace(m.Name)
		if name != "" {
			mcpNames = append(mcpNames, name)
		}
	}
	sort.Strings(mcpNames)

	caps := s.getClientCapabilities()
	configs := []SessionConfigOptionUpdate{
		{
			Name:        "cwd",
			Description: "Current working directory for this session",
			Category:    "runtime",
			Value:       snapshot.cwd,
		},
		{
			Name:        "mcp_servers",
			Description: "Bound MCP server names in this session",
			Category:    "runtime",
			Value:       mcpNames,
		},
		{
			Name:        "fs_read_text_file_enabled",
			Description: "Whether ACP client supports fs/read_text_file",
			Category:    "capability",
			Value:       caps.FS.ReadTextFile,
		},
		{
			Name:        "fs_write_text_file_enabled",
			Description: "Whether ACP client supports fs/write_text_file",
			Category:    "capability",
			Value:       caps.FS.WriteTextFile,
		},
		{
			Name:        "terminal_enabled",
			Description: "Whether ACP client supports terminal/* methods",
			Category:    "capability",
			Value:       caps.Terminal,
		},
	}

	return updates.ConfigOptions(configs)
}

func (s *Server) getSession(id string) *sessionState {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.sessions[id]
}

func (s *Server) cancelSession(sessionID string) {
	s.cancelMu.Lock()
	s.cancelFlags[sessionID] = true
	cancel, ok := s.activeCancels[sessionID]
	s.cancelMu.Unlock()

	if ok {
		cancel()
	}
}

func (s *Server) setActiveCancel(sessionID string, cancel context.CancelFunc) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	s.activeCancels[sessionID] = cancel
}

func (s *Server) clearActiveCancel(sessionID string) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	delete(s.activeCancels, sessionID)
}

func (s *Server) clearCancelFlag(sessionID string) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	delete(s.cancelFlags, sessionID)
}

func (s *Server) consumeCancelFlag(sessionID string) bool {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	v := s.cancelFlags[sessionID]
	delete(s.cancelFlags, sessionID)
	return v
}

func (s *Server) handleClientResponse(msg incomingMessage) {
	key := rpcIDKey(msg.ID)
	if key == "" {
		return
	}

	s.pendingMu.Lock()
	ch, ok := s.pendingResponses[key]
	if ok {
		delete(s.pendingResponses, key)
	}
	if !ok {
		s.earlyResponses[key] = msg
	}
	s.pendingMu.Unlock()

	if ok {
		ch <- msg
	}
}

func extractPromptText(blocks []ContentBlock) string {
	lines := make([]string, 0, len(blocks))
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		if blockType != "text" {
			continue
		}
		text, _ := block["text"].(string)
		text = strings.TrimSpace(text)
		if text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n")
}

func cloneContentBlocks(in []ContentBlock) []ContentBlock {
	out := make([]ContentBlock, 0, len(in))
	for _, block := range in {
		clone := make(ContentBlock, len(block))
		for k, v := range block {
			clone[k] = v
		}
		out = append(out, clone)
	}
	return out
}

func decodeParams(raw json.RawMessage, out any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}

func strPtr(v string) *string {
	return &v
}

func formatRFC3339UTC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
