package dataplane

import (
	"bytes"
	"net/http"
)

// retryResponseWriter buffers everything a proxied attempt writes —
// status, headers, and body — instead of sending it straight to the
// client. This lets serveOnce discard a failed attempt entirely and retry
// against a different backend without the client having seen any partial
// or error output. On a successful attempt the buffered response is
// replayed verbatim to the real ResponseWriter via flushTo.
//
// It only ever holds one attempt's worth of a bounded response (retries
// are restricted to bodyless idempotent requests, whose responses are
// typically small), so buffering here does not introduce unbounded memory
// growth in normal operation.
type retryResponseWriter struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func newRetryResponseWriter() *retryResponseWriter {
	return &retryResponseWriter{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (rw *retryResponseWriter) Header() http.Header {
	return rw.header
}

func (rw *retryResponseWriter) WriteHeader(statusCode int) {
	if rw.wroteHeader {
		return
	}
	rw.statusCode = statusCode
	rw.wroteHeader = true
}

func (rw *retryResponseWriter) Write(b []byte) (int, error) {
	rw.wroteHeader = true
	return rw.body.Write(b)
}

// flushTo replays the buffered status, headers, and body to the real
// ResponseWriter. Called only for a successful attempt.
func (rw *retryResponseWriter) flushTo(w http.ResponseWriter) {
	dst := w.Header()
	for k, vs := range rw.header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(rw.statusCode)
	_, _ = w.Write(rw.body.Bytes())
}
