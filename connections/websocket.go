package connections

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
)

// WebSocketConn abstracts one text-message websocket connection.
//
// You can adapt this interface from your preferred websocket library.
type WebSocketConn interface {
	ReadText(ctx context.Context) (string, error)
	WriteText(ctx context.Context, text string) error
	Close() error
}

// ServeWebSocket bridges ACP NDJSON stream with a websocket text-message stream.
//
// Each inbound websocket text message is treated as one JSON-RPC line.
// Each outbound JSON-RPC line from ACP is emitted as one websocket text message.
func ServeWebSocket(ctx context.Context, endpoint *Endpoint, conn WebSocketConn) error {
	if endpoint == nil {
		return errors.New("connections: endpoint is nil")
	}
	if conn == nil {
		return errors.New("connections: websocket conn is nil")
	}

	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer inWriter.Close()

		for {
			msg, err := conn.ReadText(ctx)
			if err != nil {
				if isConnClosedError(err) || errors.Is(err, context.Canceled) {
					return
				}
				recordErr(err)
				return
			}

			if _, err := io.WriteString(inWriter, strings.TrimRight(msg, "\n")+"\n"); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					recordErr(err)
				}
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		scanner := bufio.NewScanner(outReader)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			if err := conn.WriteText(ctx, scanner.Text()); err != nil {
				if isConnClosedError(err) || errors.Is(err, context.Canceled) {
					return
				}
				recordErr(err)
				return
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			recordErr(err)
		}
	}()

	serveErr := endpoint.Serve(ctx, inReader, outWriter)
	_ = outWriter.Close()
	_ = conn.Close()
	_ = inWriter.Close()
	wg.Wait()

	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		recordErr(serveErr)
	}

	return firstErr
}

func isConnClosedError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}
