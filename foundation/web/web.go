// Package web holds the HTTP middleware the status server is wrapped in.
//
// It lives in foundation rather than beside the app because it is about
// requests rather than about backups, and because a second app on this
// listener — a restore browser, say — must get the same headers without
// copying them.
package web

import (
	"log/slog"
	"net/http"
	"time"
)

// Logging wraps h to emit one line per request. A nil logger disables it,
// which is what tests want.
func Logging(log *slog.Logger, now func() time.Time, h http.Handler) http.Handler {
	if log == nil {
		return h
	}

	if now == nil {
		now = time.Now
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		h.ServeHTTP(rec, r)

		// The query string is deliberately not logged. The settings form puts
		// directory paths in the request, and this log is a file that outlives
		// the request in a user profile.
		log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", now().Sub(started),
		)
	})
}

// SecureHeaders applies the response headers the status page relies on.
//
// The Content-Security-Policy is as strict as it is because the page is
// server-rendered and has no scripts and no remote assets at all. A page that
// cannot make a network request cannot leak a directory listing, whatever ends
// up injected into a filename shown on it — and filenames are exactly what
// this page displays, from a machine's own disk, unvalidated by anybody.
func SecureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		head.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; img-src 'self' data:; "+
				"form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		head.Set("X-Content-Type-Options", "nosniff")
		// same-origin rather than no-referrer, which is what this was and
		// which broke every form on this server in Firefox.
		//
		// The Fetch standard says that when a request's referrer policy is
		// no-referrer, the Origin header on an unsafe method is serialised as
		// "null". Firefox implements that; Chrome does not. So a POST from
		// this server's own setup page arrived with Origin: null, the loopback
		// guard refused it as coming from somewhere else, and the person who
		// had just filled the page in got "a request from null cannot change
		// this machine's backup settings".
		//
		// same-origin gives up nothing here: the referrer is still withheld
		// from every other origin, and this page has no cross-origin requests
		// to withhold it from — the policy above forbids them all.
		head.Set("Referrer-Policy", "same-origin")
		head.Set("Cache-Control", "no-store, max-age=0")

		h.ServeHTTP(w, r)
	})
}

// statusRecorder remembers what was written, for the log line.
type statusRecorder struct {
	http.ResponseWriter

	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}

	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true

	return s.ResponseWriter.Write(b)
}
