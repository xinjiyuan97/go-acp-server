package acpserver

import (
	"context"
	"fmt"
	"strings"
)

func (s *Server) writeSessionUpdate(sessionID, text string) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate": sessionUpdateAgentMessageChunk,
		"content": map[string]any{
			"type": "text",
			"text": text,
		},
	})
}

func (s *Server) writeUserMessageUpdate(sessionID, text string) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate": sessionUpdateUserMessageChunk,
		"content": map[string]any{
			"type": "text",
			"text": text,
		},
	})
}

func (s *Server) writeAgentThoughtUpdate(sessionID, text string) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate": sessionUpdateAgentThoughtChunk,
		"content": map[string]any{
			"type": "text",
			"text": text,
		},
	})
}

func (s *Server) writePlanUpdate(sessionID string, entries []PlanEntryUpdate) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate": sessionUpdatePlan,
		"entries":       entries,
	})
}

func (s *Server) writeAvailableCommandsUpdate(sessionID string, commands []AvailableCommandUpdate) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate":     sessionUpdateAvailableCommands,
		"availableCommands": commands,
	})
}

func (s *Server) writeCurrentModeUpdate(sessionID, modeID string) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate": sessionUpdateCurrentMode,
		"currentModeId": modeID,
	})
}

func (s *Server) writeConfigOptionUpdate(sessionID string, options []SessionConfigOptionUpdate) {
	s.writeSessionUpdateRaw(sessionID, map[string]any{
		"sessionUpdate": sessionUpdateConfigOption,
		"configOptions": options,
	})
}

func (s *Server) writeSessionInfoUpdate(sessionID, title, updatedAt string) {
	update := map[string]any{
		"sessionUpdate": sessionUpdateSessionInfoUpdate,
	}
	if strings.TrimSpace(title) != "" {
		update["title"] = title
	}
	if strings.TrimSpace(updatedAt) != "" {
		update["updatedAt"] = updatedAt
	}
	s.writeSessionUpdateRaw(sessionID, update)
}

func (s *Server) writeSessionUpdateRaw(sessionID string, update any) {
	notification := outgoingNotification{
		JSONRPC: "2.0",
		Method:  methodSessionUpdate,
		Params: map[string]any{
			"sessionId": sessionID,
			"update":    update,
		},
	}

	if err := s.writer.write(notification); err != nil {
		s.logf("failed to write session update: %v", err)
	}
}

type promptUpdateWriter struct {
	server      *Server
	sessionID   string
	ctx         context.Context
	onAgentText func()
}

func newPromptUpdateWriter(
	server *Server,
	sessionID string,
	ctx context.Context,
	onAgentText func(),
) *promptUpdateWriter {
	return &promptUpdateWriter{
		server:      server,
		sessionID:   sessionID,
		ctx:         ctx,
		onAgentText: onAgentText,
	}
}

func (w *promptUpdateWriter) UserMessageChunk(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := w.assertContext(); err != nil {
		return err
	}
	w.server.writeUserMessageUpdate(w.sessionID, text)
	return nil
}

func (w *promptUpdateWriter) AgentMessageChunk(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := w.assertContext(); err != nil {
		return err
	}
	if w.onAgentText != nil {
		w.onAgentText()
	}
	w.server.writeSessionUpdate(w.sessionID, text)
	return nil
}

func (w *promptUpdateWriter) AgentThoughtChunk(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := w.assertContext(); err != nil {
		return err
	}
	w.server.writeAgentThoughtUpdate(w.sessionID, text)
	return nil
}

func (w *promptUpdateWriter) Plan(entries []PlanEntryUpdate) error {
	if err := w.assertContext(); err != nil {
		return err
	}
	w.server.writePlanUpdate(w.sessionID, entries)
	return nil
}

func (w *promptUpdateWriter) AvailableCommands(commands []AvailableCommandUpdate) error {
	if err := w.assertContext(); err != nil {
		return err
	}
	w.server.writeAvailableCommandsUpdate(w.sessionID, commands)
	return nil
}

func (w *promptUpdateWriter) CurrentMode(modeID string) error {
	if strings.TrimSpace(modeID) == "" {
		return fmt.Errorf("current mode id is required")
	}
	if err := w.assertContext(); err != nil {
		return err
	}
	w.server.writeCurrentModeUpdate(w.sessionID, modeID)
	return nil
}

func (w *promptUpdateWriter) ConfigOptions(options []SessionConfigOptionUpdate) error {
	if err := w.assertContext(); err != nil {
		return err
	}
	w.server.writeConfigOptionUpdate(w.sessionID, options)
	return nil
}

func (w *promptUpdateWriter) Raw(update map[string]any) error {
	if err := w.assertContext(); err != nil {
		return err
	}
	if update == nil {
		return fmt.Errorf("update payload is required")
	}
	sessionUpdate, _ := update["sessionUpdate"].(string)
	if strings.TrimSpace(sessionUpdate) == "" {
		return fmt.Errorf("update.sessionUpdate is required")
	}
	w.server.writeSessionUpdateRaw(w.sessionID, update)
	return nil
}

func (w *promptUpdateWriter) assertContext() error {
	if w.ctx == nil {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	return nil
}
