package acptest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	acpserver "github.com/xinjiyuan97/go-acp-server"
)

const defaultRequestTimeout = 5 * time.Second

// RPCError represents a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Message is one JSON-RPC line emitted by the ACP server.
type Message struct {
	Seq     int
	Raw     string
	Payload map[string]any
	ID      any
	Method  string
	Params  map[string]any
	Result  any
	Error   map[string]any
}

// ClientRequest is a server->client JSON-RPC request.
type ClientRequest struct {
	Message
}

// ClientReply is the JSON-RPC response sent back to the server.
type ClientReply struct {
	Result any
	Error  *RPCError
}

// ClientRequestHandler allows dynamic stubbing for server->client requests.
// Return handled=false to fall back to queued/default behavior.
type ClientRequestHandler func(req ClientRequest) (reply ClientReply, handled bool)

// ToolEvent is a parsed tool progress event from session/update.
type ToolEvent struct {
	UpdateType string
	ToolCallID string
	Status     string
	Title      string
	Kind       string
	RawInput   any
	RawOutput  any
	Content    any
}

// Exchange captures one request/response round plus all emitted messages
// from request dispatch until matching response is received.
type Exchange struct {
	RequestID any
	Messages  []Message
	Response  Message
}

// Turn is a parsed prompt exchange with convenient aggregates.
type Turn struct {
	Exchange
	SessionID     string
	StopReason    string
	UserText      string
	AssistantText string
	Updates       []map[string]any
	ToolEvents    []ToolEvent
}

// Format renders a readable summary for debugging assertions.
func (t Turn) Format() string {
	var b strings.Builder
	if strings.TrimSpace(t.SessionID) != "" {
		fmt.Fprintf(&b, "Session: %s\n", t.SessionID)
	}
	if strings.TrimSpace(t.StopReason) != "" {
		fmt.Fprintf(&b, "StopReason: %s\n", t.StopReason)
	}
	if strings.TrimSpace(t.UserText) != "" {
		b.WriteString("User:\n")
		b.WriteString(strings.TrimSpace(t.UserText))
		b.WriteString("\n")
	}
	if strings.TrimSpace(t.AssistantText) != "" {
		b.WriteString("Assistant:\n")
		b.WriteString(strings.TrimSpace(t.AssistantText))
		b.WriteString("\n")
	}
	if len(t.ToolEvents) > 0 {
		b.WriteString("Tools:\n")
		for _, ev := range t.ToolEvents {
			kind := strings.TrimSpace(ev.UpdateType)
			if strings.TrimSpace(ev.Status) != "" {
				kind += "/" + ev.Status
			}
			fmt.Fprintf(&b, "- [%s] id=%s", kind, ev.ToolCallID)
			if strings.TrimSpace(ev.Title) != "" {
				fmt.Fprintf(&b, " title=%s", ev.Title)
			}
			if strings.TrimSpace(ev.Kind) != "" {
				fmt.Fprintf(&b, " kind=%s", ev.Kind)
			}
			b.WriteString("\n")
			if ev.RawInput != nil {
				fmt.Fprintf(&b, "  input: %s\n", prettyJSON(ev.RawInput))
			}
			if ev.RawOutput != nil {
				fmt.Fprintf(&b, "  output: %s\n", prettyJSON(ev.RawOutput))
			}
		}
	}
	if b.Len() == 0 {
		return "(empty turn)"
	}
	return strings.TrimSpace(b.String())
}

// Option customizes driver behavior.
type Option func(*Driver)

// WithTimeout configures request/response timeout for Do.
func WithTimeout(timeout time.Duration) Option {
	return func(d *Driver) {
		if timeout > 0 {
			d.timeout = timeout
		}
	}
}

// WithClientHandler installs a dynamic handler for server->client requests.
func WithClientHandler(handler ClientRequestHandler) Option {
	return func(d *Driver) {
		d.clientHandler = handler
	}
}

// Driver is a persistent ACP test harness for multi-turn conversations.
type Driver struct {
	ctx    context.Context
	cancel context.CancelFunc

	srv *acpserver.Server

	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter

	serveDone chan error
	readDone  chan error

	writeMu sync.Mutex

	mu sync.Mutex
	// Sequence for emitted server lines.
	seq int
	// All emitted lines.
	messages []Message
	// pending request response channels by rpc id key.
	pending map[string]chan Message
	// next numeric id for client->server requests.
	nextID int

	currentSessionID string

	// queued replies by method for outbound server->client requests.
	clientQueues  map[string][]ClientReply
	clientHandler ClientRequestHandler

	timeout time.Duration
	closed  bool
}

// NewDriver starts a persistent ACP server loop for test driving.
func NewDriver(server *acpserver.Server, opts ...Option) *Driver {
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	d := &Driver{
		ctx:          ctx,
		cancel:       cancel,
		srv:          server,
		inR:          inR,
		inW:          inW,
		outR:         outR,
		outW:         outW,
		serveDone:    make(chan error, 1),
		readDone:     make(chan error, 1),
		pending:      make(map[string]chan Message),
		nextID:       1,
		clientQueues: make(map[string][]ClientReply),
		timeout:      defaultRequestTimeout,
	}
	for _, opt := range opts {
		opt(d)
	}

	go func() {
		err := d.srv.Serve(d.ctx, d.inR, d.outW)
		_ = d.outW.Close()
		d.serveDone <- err
	}()
	go d.readLoop()

	return d
}

// Close stops background goroutines and closes pipes.
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	d.cancel()
	_ = d.inW.Close()

	var serveErr, readErr error
	select {
	case serveErr = <-d.serveDone:
	case <-time.After(d.timeout):
		serveErr = fmt.Errorf("timeout waiting for server stop")
	}
	select {
	case readErr = <-d.readDone:
	case <-time.After(d.timeout):
		readErr = fmt.Errorf("timeout waiting for read loop stop")
	}
	if serveErr != nil && serveErr != context.Canceled {
		return serveErr
	}
	if readErr != nil && readErr != io.EOF && readErr != context.Canceled {
		return readErr
	}
	return nil
}

// QueueClientResult enqueues a method-specific result for outbound server->client RPC.
func (d *Driver) QueueClientResult(method string, result any) {
	d.QueueClientReply(method, ClientReply{Result: result})
}

// QueueClientError enqueues a method-specific JSON-RPC error.
func (d *Driver) QueueClientError(method string, code int, message string) {
	d.QueueClientReply(method, ClientReply{Error: &RPCError{Code: code, Message: message}})
}

// QueueClientReply enqueues a full method-specific outbound reply.
func (d *Driver) QueueClientReply(method string, reply ClientReply) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clientQueues[method] = append(d.clientQueues[method], reply)
}

// SetCurrentSession sets default session id for PromptCurrent.
func (d *Driver) SetCurrentSession(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.currentSessionID = strings.TrimSpace(sessionID)
}

// CurrentSession returns current default session id.
func (d *Driver) CurrentSession() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.currentSessionID
}

// Messages returns all server-emitted messages so far.
func (d *Driver) Messages() []Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Message, len(d.messages))
	copy(out, d.messages)
	return out
}

// Initialize sends initialize and returns full exchange.
func (d *Driver) Initialize(protocolVersion int, clientCapabilities map[string]any) (Exchange, error) {
	params := map[string]any{"protocolVersion": protocolVersion}
	if clientCapabilities != nil {
		params["clientCapabilities"] = clientCapabilities
	}
	return d.Do("initialize", params)
}

// NewSession sends session/new and remembers session id as current.
func (d *Driver) NewSession(cwd string, mcpServers []map[string]any) (string, Exchange, error) {
	params := map[string]any{"cwd": cwd}
	if len(mcpServers) > 0 {
		params["mcpServers"] = mcpServers
	}
	ex, err := d.Do("session/new", params)
	if err != nil {
		return "", ex, err
	}
	result, ok := ex.Response.Result.(map[string]any)
	if !ok {
		return "", ex, fmt.Errorf("session/new missing result object")
	}
	sessionID, _ := result["sessionId"].(string)
	if strings.TrimSpace(sessionID) == "" {
		return "", ex, fmt.Errorf("session/new missing sessionId")
	}
	d.SetCurrentSession(sessionID)
	return sessionID, ex, nil
}

// PromptCurrent sends session/prompt using current session id.
func (d *Driver) PromptCurrent(blocks []map[string]any) (Turn, error) {
	sessionID := d.CurrentSession()
	if strings.TrimSpace(sessionID) == "" {
		return Turn{}, fmt.Errorf("current session is empty")
	}
	return d.Prompt(sessionID, blocks)
}

// PromptTextCurrent is PromptCurrent for one text block.
func (d *Driver) PromptTextCurrent(text string) (Turn, error) {
	return d.PromptCurrent([]map[string]any{{"type": "text", "text": text}})
}

// Prompt sends session/prompt for one session.
func (d *Driver) Prompt(sessionID string, blocks []map[string]any) (Turn, error) {
	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    blocks,
	}
	ex, err := d.Do("session/prompt", params)
	if err != nil {
		return Turn{}, err
	}
	return parseTurn(ex, sessionID), nil
}

// Do sends one JSON-RPC request and waits for matching response.
func (d *Driver) Do(method string, params any) (Exchange, error) {
	requestID := d.nextRequestID()
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  params,
	}

	respCh := make(chan Message, 1)
	key := rpcIDKey(requestID)

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return Exchange{}, fmt.Errorf("driver is closed")
	}
	startSeq := d.seq
	d.pending[key] = respCh
	d.mu.Unlock()

	if err := d.writeLine(request); err != nil {
		d.mu.Lock()
		delete(d.pending, key)
		d.mu.Unlock()
		return Exchange{}, err
	}

	var resp Message
	select {
	case resp = <-respCh:
	case <-time.After(d.timeout):
		d.mu.Lock()
		delete(d.pending, key)
		d.mu.Unlock()
		return Exchange{}, fmt.Errorf("timeout waiting response for method %s id=%v", method, requestID)
	case <-d.ctx.Done():
		return Exchange{}, d.ctx.Err()
	}

	d.mu.Lock()
	msgs := messagesInRange(d.messages, startSeq+1, resp.Seq)
	d.mu.Unlock()

	ex := Exchange{
		RequestID: requestID,
		Messages:  msgs,
		Response:  resp,
	}
	if resp.Error != nil {
		return ex, fmt.Errorf("json-rpc error code=%v message=%v", resp.Error["code"], resp.Error["message"])
	}
	return ex, nil
}

func (d *Driver) readLoop() {
	scanner := bufio.NewScanner(d.outR)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		msg, err := decodeMessage(line)
		if err != nil {
			// Keep malformed line for debugging but continue scanning.
			msg = Message{Raw: line, Payload: map[string]any{"_decodeError": err.Error()}}
		}

		var respCh chan Message
		var outboundReq *ClientRequest

		d.mu.Lock()
		d.seq++
		msg.Seq = d.seq
		d.messages = append(d.messages, msg)

		if msg.Method != "" && msg.ID != nil {
			req := ClientRequest{Message: msg}
			outboundReq = &req
		}
		if msg.Method == "" && msg.ID != nil {
			key := rpcIDKey(msg.ID)
			if ch, ok := d.pending[key]; ok {
				respCh = ch
				delete(d.pending, key)
			}
		}
		d.mu.Unlock()

		if outboundReq != nil {
			d.replyToClientRequest(*outboundReq)
		}
		if respCh != nil {
			respCh <- msg
		}
	}

	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	d.readDone <- err
}

func (d *Driver) replyToClientRequest(req ClientRequest) {
	reply, _ := d.resolveClientReply(req)

	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
	}
	if reply.Error != nil {
		response["error"] = map[string]any{
			"code":    reply.Error.Code,
			"message": reply.Error.Message,
		}
	} else {
		response["result"] = reply.Result
	}

	_ = d.writeLine(response)
}

func (d *Driver) resolveClientReply(req ClientRequest) (ClientReply, bool) {
	d.mu.Lock()
	handler := d.clientHandler
	d.mu.Unlock()
	if handler != nil {
		if reply, handled := handler(req); handled {
			return reply, true
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	queue := d.clientQueues[req.Method]
	if len(queue) == 0 {
		return ClientReply{Result: nil}, false
	}
	reply := queue[0]
	d.clientQueues[req.Method] = queue[1:]
	return reply, true
}

func (d *Driver) nextRequestID() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.nextID
	d.nextID++
	return id
}

func (d *Driver) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err = d.inW.Write(b)
	return err
}

func parseTurn(ex Exchange, sessionID string) Turn {
	turn := Turn{
		Exchange:  ex,
		SessionID: sessionID,
	}

	if result, ok := ex.Response.Result.(map[string]any); ok {
		if stop, _ := result["stopReason"].(string); stop != "" {
			turn.StopReason = stop
		}
	}

	var userTexts []string
	var agentTexts []string

	for _, msg := range ex.Messages {
		if msg.Method != "session/update" {
			continue
		}
		update := updatePayload(msg)
		if update == nil {
			continue
		}
		turn.Updates = append(turn.Updates, update)

		kind, _ := update["sessionUpdate"].(string)
		switch kind {
		case "user_message_chunk":
			if text := contentText(update["content"]); text != "" {
				userTexts = append(userTexts, text)
			}
		case "agent_message_chunk":
			if text := contentText(update["content"]); text != "" {
				agentTexts = append(agentTexts, text)
			}
		case "tool_call", "tool_call_update":
			turn.ToolEvents = append(turn.ToolEvents, ToolEvent{
				UpdateType: kind,
				ToolCallID: stringValue(update, "toolCallId"),
				Status:     stringValue(update, "status"),
				Title:      stringValue(update, "title"),
				Kind:       stringValue(update, "kind"),
				RawInput:   update["rawInput"],
				RawOutput:  update["rawOutput"],
				Content:    update["content"],
			})
		}
	}

	turn.UserText = strings.Join(userTexts, "")
	turn.AssistantText = strings.Join(agentTexts, "")
	return turn
}

func updatePayload(msg Message) map[string]any {
	if msg.Params == nil {
		return nil
	}
	update, _ := msg.Params["update"].(map[string]any)
	if update == nil {
		return nil
	}
	return update
}

func contentText(v any) string {
	content, _ := v.(map[string]any)
	if content == nil {
		return ""
	}
	text, _ := content["text"].(string)
	return text
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func decodeMessage(line string) (Message, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return Message{}, err
	}
	msg := Message{
		Raw:     line,
		Payload: payload,
	}
	if id, ok := payload["id"]; ok {
		msg.ID = id
	}
	if method, ok := payload["method"].(string); ok {
		msg.Method = method
	}
	if params, ok := payload["params"].(map[string]any); ok {
		msg.Params = params
	}
	if result, ok := payload["result"]; ok {
		msg.Result = result
	}
	if errObj, ok := payload["error"].(map[string]any); ok {
		msg.Error = errObj
	}
	return msg, nil
}

func messagesInRange(in []Message, startSeq, endSeq int) []Message {
	if startSeq > endSeq {
		return nil
	}
	out := make([]Message, 0, endSeq-startSeq+1)
	for _, msg := range in {
		if msg.Seq < startSeq || msg.Seq > endSeq {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func rpcIDKey(id any) string {
	switch v := id.(type) {
	case nil:
		return "null"
	case string:
		return "s:" + v
	case int:
		return "n:" + strconv.Itoa(v)
	case int32:
		return "n:" + strconv.FormatInt(int64(v), 10)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	case float64:
		if v == math.Trunc(v) {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return "n:" + strconv.FormatFloat(v, 'g', -1, 64)
	case json.Number:
		return "n:" + v.String()
	default:
		b, _ := json.Marshal(v)
		return "j:" + string(b)
	}
}

func prettyJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
