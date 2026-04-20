package connections

import (
	"context"
	"errors"
	"io"
	"sync"

	acpserver "github.com/Pudbot-BeanAI/fucking-agents/crates/go-acp-server"
)

// Endpoint serializes calls into one ACP server instance.
//
// acpserver.Server keeps request writer state internally, so concurrent Serve
// calls on the same instance are not safe. Endpoint provides a simple lock to
// keep transport adapters (stdio/http/websocket) safe when shared.
type Endpoint struct {
	server *acpserver.Server
	mu     sync.Mutex
}

func NewEndpoint(server *acpserver.Server) *Endpoint {
	return &Endpoint{server: server}
}

func (e *Endpoint) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if e == nil || e.server == nil {
		return errors.New("connections: endpoint server is nil")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.server.Serve(ctx, in, out)
}
