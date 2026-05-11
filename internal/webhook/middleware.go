package webhook

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
)

// ErrHijackNotSupported is returned by [statusRecorder.Hijack] when the wrapped
// [http.ResponseWriter] does not implement [http.Hijacker].
var ErrHijackNotSupported = errors.New("webhook: underlying ResponseWriter does not support hijacking")

// requireToken returns middleware that 401s any request whose
// "Authorization: Bearer <token>" header does not equal secret. The scheme
// match is case-insensitive per RFC 7235; the token comparison is constant
// time.
func requireToken(secret string, lg *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
			lg.Warn("rejected unauthenticated request", "path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="mithras"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken parses the value of the Authorization header and returns the
// token portion when the scheme is Bearer. The second return is false when
// the header is missing, malformed, or uses a different scheme.
func bearerToken(authHeader string) (string, bool) {
	const prefix = "Bearer "
	if len(authHeader) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(authHeader[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// recover500 converts panics in downstream handlers into a 500 response and
// logs the panic value with its stack trace.
func recover500(lg *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				lg.Error("panic in handler",
					"err", rec,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLog emits one structured log line per request.
func requestLog(lg *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		lg.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"remote", r.RemoteAddr,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, ErrHijackNotSupported
}
