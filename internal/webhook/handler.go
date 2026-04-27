// Package webhook implements the HTTP-facing pieces of the webhookd binary:
// middleware, request handling, and per-request agent wiring. Configuration
// loading lives in the [webhookconfig] subpackage.
package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// Launcher is the subset of [*BackgroundLauncher] the handler depends on. It
// exists so tests can stub it out.
type Launcher interface {
	Launch(requestID, prompt string)
}

// InvokeResponse is the body returned by POST /v1/invoke.
type InvokeResponse struct {
	RequestID string `json:"requestID"`
}

// Router returns an http.Handler wired up for the webhookd service.
//
// secret is the shared value required in the X-Mithras-Token header on
// /v1/invoke. maxBody caps request bodies (in bytes).
func Router(launcher Launcher, secret string, maxBody int64, lg *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	invoke := invokeHandler(launcher, maxBody, lg)
	authed := requireToken(secret, lg, invoke)

	mux.Handle("POST /v1/invoke", authed)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	var h http.Handler = mux
	h = recover500(lg, h)
	h = requestLog(lg, h)
	return h
}

func invokeHandler(launcher Launcher, maxBody int64, lg *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if ct != "application/json" {
			http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
				return
			}
			lg.Warn("read body failed", "err", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Validate the body is JSON but preserve it verbatim: re-marshaling
		// through any loses large-integer precision and reorders object keys.
		if !json.Valid(body) {
			http.Error(w, "Bad Request: body must be JSON", http.StatusBadRequest)
			return
		}

		requestID := uuid.Must(uuid.NewV7()).String()
		launcher.Launch(requestID, string(body))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(InvokeResponse{RequestID: requestID}); err != nil {
			lg.Warn("encode response failed", "err", err)
		}
	})
}
