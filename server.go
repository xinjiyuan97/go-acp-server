package acpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	protocolVersionV1 = 1

	methodInitialize    = "initialize"
	methodAuthenticate  = "authenticate"
	methodSessionNew    = "session/new"
	methodSessionLoad   = "session/load"
	methodSessionList   = "session/list"
	methodSessionPrompt = "session/prompt"
	methodSessionCancel = "session/cancel"
	methodSessionUpdate = "session/update"

	stopReasonEndTurn   = "end_turn"
	stopReasonCancelled = "cancelled"

	sessionUpdateUserMessageChunk  = "user_message_chunk"
	sessionUpdateAgentMessageChunk = "agent_message_chunk"
	sessionUpdateAgentThoughtChunk = "agent_thought_chunk"
	sessionUpdatePlan              = "plan"
	sessionUpdateAvailableCommands = "available_commands_update"
	sessionUpdateCurrentMode       = "current_mode_update"
	sessionUpdateConfigOption      = "config_option_update"
	sessionUpdateSessionInfoUpdate = "session_info_update"
)

const (
	errParseError     = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternalError  = -32603
)

// ChatTurn stores one user/assistant exchange inside a session.
type ChatTurn struct {
	User      string
	Assistant string
}

// PromptExecutor executes one ACP prompt turn.
type PromptExecutor interface {
	StreamReply(
		ctx context.Context,
		history []ChatTurn,
		prompt string,
		tools RuntimeToolInvoker,
		onChunk func(chunk string) error,
	) (string, error)
}

// PromptExecutorWithUpdates can stream rich ACP session/update events.
//
// If implemented, the server will prefer this method over PromptExecutor.StreamReply.
type PromptExecutorWithUpdates interface {
	PromptExecutor
	StreamReplyWithUpdates(
		ctx context.Context,
		history []ChatTurn,
		prompt string,
		tools RuntimeToolInvoker,
		updates PromptUpdateWriter,
	) (string, error)
}

// PromptUpdateWriter emits ACP session/update notifications.
type PromptUpdateWriter interface {
	UserMessageChunk(text string) error
	AgentMessageChunk(text string) error
	AgentThoughtChunk(text string) error
	Plan(entries []PlanEntryUpdate) error
	AvailableCommands(commands []AvailableCommandUpdate) error
	CurrentMode(modeID string) error
	ConfigOptions(options []SessionConfigOptionUpdate) error
	Raw(update map[string]any) error
}

// PlanEntryUpdate is one entry in a plan update.
type PlanEntryUpdate struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// AvailableCommandUpdate is one command in available_commands_update.
type AvailableCommandUpdate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Input       any    `json:"input,omitempty"`
}

// SessionConfigOptionUpdate is one option in config_option_update.
type SessionConfigOptionUpdate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Value       any    `json:"value,omitempty"`
}

type Server struct {
	executor PromptExecutor
	version  string
	logger   io.Writer
	writer   *rpcWriter

	sessionsMu sync.RWMutex
	sessions   map[string]*sessionState

	clientCapsMu sync.RWMutex
	clientCaps   clientCapabilities

	cancelMu      sync.Mutex
	cancelFlags   map[string]bool
	activeCancels map[string]context.CancelFunc

	requestIDCounter uint64
	pendingMu        sync.Mutex
	pendingResponses map[string]chan incomingMessage
	earlyResponses   map[string]incomingMessage
}

type sessionState struct {
	CWD string
	// Title is optional and used for session/list + session_info_update.
	Title string

	mu         sync.Mutex
	history    []ChatTurn
	mcpServers []mcpServer
	updatedAt  time.Time
}

type sessionSnapshot struct {
	cwd        string
	mcpServers []mcpServer
}

func NewServer(executor PromptExecutor, logger io.Writer, version string) *Server {
	return &Server{
		executor:         executor,
		version:          version,
		logger:           logger,
		sessions:         make(map[string]*sessionState),
		cancelFlags:      make(map[string]bool),
		activeCancels:    make(map[string]context.CancelFunc),
		pendingResponses: make(map[string]chan incomingMessage),
		earlyResponses:   make(map[string]incomingMessage),
	}
}

func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.writer = newRPCWriter(out)

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var wg sync.WaitGroup

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg incomingMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			s.writeError(nil, errParseError, fmt.Sprintf("failed to parse JSON-RPC message: %v", err))
			continue
		}

		if msg.JSONRPC != "2.0" {
			if msg.hasID() {
				s.writeError(msg.ID, errInvalidRequest, "jsonrpc must be '2.0'")
			}
			continue
		}

		if strings.TrimSpace(msg.Method) == "" {
			if msg.hasID() {
				s.handleClientResponse(msg)
			}
			continue
		}

		if msg.hasID() {
			wg.Add(1)
			go func(req incomingMessage) {
				defer wg.Done()
				s.handleRequest(ctx, req)
			}(msg)
			continue
		}

		s.handleNotification(msg)
	}

	wg.Wait()
	return scanner.Err()
}

func (s *Server) nextRequestID() string {
	id := atomic.AddUint64(&s.requestIDCounter, 1)
	return fmt.Sprintf("acp-client-%d", id)
}

func (s *Server) registerPendingResponse(id string) chan incomingMessage {
	key := "s:" + id
	ch := make(chan incomingMessage, 1)

	s.pendingMu.Lock()
	if early, ok := s.earlyResponses[key]; ok {
		delete(s.earlyResponses, key)
		s.pendingMu.Unlock()
		ch <- early
		return ch
	}

	s.pendingResponses[key] = ch
	s.pendingMu.Unlock()
	return ch
}

func (s *Server) unregisterPendingResponse(id string) {
	key := "s:" + id
	s.pendingMu.Lock()
	delete(s.pendingResponses, key)
	s.pendingMu.Unlock()
}

func (s *Server) callClient(ctx context.Context, method string, params any, out any) error {
	reqID := s.nextRequestID()
	respCh := s.registerPendingResponse(reqID)
	defer s.unregisterPendingResponse(reqID)

	s.logf("[acp->client] method=%s id=%s params=%s", method, reqID, compactJSON(params))

	req := outgoingRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%q", reqID)),
		Method:  method,
		Params:  params,
	}
	if err := s.writer.write(req); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case msg := <-respCh:
		if msg.Error != nil {
			s.logf("[client->acp] id=%s error_code=%d error=%s", reqID, msg.Error.Code, msg.Error.Message)
			return fmt.Errorf("client method %s failed: (%d) %s", method, msg.Error.Code, msg.Error.Message)
		}

		s.logf("[client->acp] id=%s result=%s", reqID, compactRawJSON(msg.Result))
		if out == nil || len(bytes.TrimSpace(msg.Result)) == 0 || bytes.Equal(bytes.TrimSpace(msg.Result), []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(msg.Result, out); err != nil {
			return fmt.Errorf("client method %s returned invalid result: %w", method, err)
		}
		return nil
	}
}

func (s *Server) writeResult(id json.RawMessage, result any) {
	response := outgoingSuccess{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Result:  result,
	}

	if err := s.writer.write(response); err != nil {
		s.logf("failed to write success response: %v", err)
	}
}

func (s *Server) writeError(id json.RawMessage, code int, message string) {
	response := outgoingError{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error: rpcError{
			Code:    code,
			Message: message,
		},
	}

	if err := s.writer.write(response); err != nil {
		s.logf("failed to write error response: %v", err)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	_, _ = fmt.Fprintf(s.logger, format+"\n", args...)
}

func (s *Server) buildVersion() string {
	if strings.TrimSpace(s.version) != "" {
		return s.version
	}
	return "dev"
}

func (s *Server) getClientCapabilities() clientCapabilities {
	s.clientCapsMu.RLock()
	defer s.clientCapsMu.RUnlock()
	return s.clientCaps
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(id)) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func rpcIDKey(id json.RawMessage) string {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 {
		return ""
	}

	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return "j:" + string(trimmed)
	}

	switch vv := v.(type) {
	case string:
		return "s:" + vv
	case float64:
		return fmt.Sprintf("n:%g", vv)
	case bool:
		return fmt.Sprintf("b:%t", vv)
	default:
		return "j:" + string(trimmed)
	}
}

func compactJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal_error: %v>", err)
	}
	return string(b)
}

func compactRawJSON(v json.RawMessage) string {
	if len(bytes.TrimSpace(v)) == 0 {
		return "null"
	}
	return string(v)
}

func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return "eino-" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("eino-%d", time.Now().UnixNano())
}

type rpcWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newRPCWriter(w io.Writer) *rpcWriter {
	return &rpcWriter{w: bufio.NewWriter(w)}
}

func (w *rpcWriter) write(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.w.Write(payload); err != nil {
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return err
	}
	return w.w.Flush()
}
