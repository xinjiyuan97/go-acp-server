package connections

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

const (
	defaultHTTPMaxBodyBytes = int64(4 * 1024 * 1024)
	ndjsonContentType       = "application/x-ndjson; charset=utf-8"
)

type HTTPHandlerOptions struct {
	// MaxBodyBytes limits one request body size. <= 0 means default 4MB.
	MaxBodyBytes int64
}

func NewHTTPHandler(endpoint *Endpoint, opts HTTPHandlerOptions) http.Handler {
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultHTTPMaxBodyBytes
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if endpoint == nil {
			http.Error(w, "endpoint is required", http.StatusInternalServerError)
			return
		}

		body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
		defer body.Close()

		var out bytes.Buffer
		if err := endpoint.Serve(r.Context(), body, &out); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, io.EOF) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}

		w.Header().Set("Content-Type", ndjsonContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out.Bytes())
	})
}
