//go:build devfaults

// Package devfaults injects synthetic transport faults so that an independent
// client can be driven through the failures the protocol exists to survive:
// a response lost after the write committed, a throttle, and a disconnect
// mid-response. It is excluded from ordinary builds by the devfaults build tag,
// so a release binary cannot contain it — which is a property of the build
// rather than a promise in a comment.
package devfaults

import (
	"errors"
	"net/http"
	"strings"
)

// Header names the fault a request should suffer. It is only read when the
// composition explicitly enabled injection, which the development server does
// and no other composition does.
const Header = "X-Tessera-Dev-Fault"

// Available lists the faults, so a client can be told what it may ask for
// rather than guessing at strings.
var Available = []string{"drop-response", "throttle", "disconnect"}

// Enabled reports whether this binary can inject faults at all.
func Enabled() bool { return true }

// Wrap returns a handler that injects the fault a request asks for. The
// distinction that matters is between a fault applied before the work and one
// applied after it: a throttle must not commit anything, while a dropped
// response must commit everything and then say nothing, because that is the
// case a client cannot distinguish from failure without asking.
func Wrap(next http.Handler, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimSpace(r.Header.Get(Header)) {
		case "throttle":
			// Refused before the handler runs, so nothing was written.
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"Synthetic development fault"}}`))
		case "drop-response":
			// Run the request to completion, then drop the connection without
			// writing anything. The write committed; the caller cannot know.
			next.ServeHTTP(&discard{header: http.Header{}}, r)
			hijack(w)
		case "disconnect":
			// Write a truncated prefix and then break the connection, so the
			// client sees a partial body rather than a clean failure.
			recorder := &capture{header: http.Header{}}
			next.ServeHTTP(recorder, r)
			if flusher, ok := w.(http.Flusher); ok && len(recorder.body) > 0 {
				w.Header().Set("Content-Type", recorder.header.Get("Content-Type"))
				w.WriteHeader(recorder.status)
				_, _ = w.Write(recorder.body[:len(recorder.body)/2])
				flusher.Flush()
			}
			hijack(w)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// hijack takes the connection away from the server and closes it, which is
// what makes the loss look like a network failure rather than an empty reply.
func hijack(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	connection, _, err := hijacker.Hijack()
	if err != nil && !errors.Is(err, http.ErrHijacked) {
		return
	}
	if connection != nil {
		_ = connection.Close()
	}
}

// discard swallows a response entirely.
type discard struct {
	header http.Header
}

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

// capture keeps a response so half of it can be sent.
type capture struct {
	header http.Header
	body   []byte
	status int
}

func (c *capture) Header() http.Header { return c.header }
func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	c.body = append(c.body, b...)
	return len(b), nil
}
func (c *capture) WriteHeader(status int) { c.status = status }
