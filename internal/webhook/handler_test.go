package webhook

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type stubLauncher struct {
	mu     sync.Mutex
	calls  []stubCall
}

type stubCall struct {
	requestID, prompt string
}

func (s *stubLauncher) Launch(requestID, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{requestID: requestID, prompt: prompt})
}

func (s *stubLauncher) Calls() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall{}, s.calls...)
}

func newTestRouter(t *testing.T, secret string, maxBody int64) (*stubLauncher, http.Handler) {
	t.Helper()
	launcher := &stubLauncher{}
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	return launcher, Router(launcher, secret, maxBody, lg)
}

func TestInvokeHandler(t *testing.T) {
	t.Parallel()

	const secret = "s3kret"

	tests := []struct {
		name         string
		method       string
		path         string
		contentType  string
		token        string
		body         string
		wantStatus   int
		wantLaunched bool
		checkPrompt  func(t *testing.T, prompt string)
	}{
		{
			name:        "happy path",
			method:      http.MethodPost,
			path:        "/v1/invoke",
			contentType: "application/json",
			token:       secret,
			body:        `{"event":"push","repo":"mithras"}`,
			wantStatus:  http.StatusAccepted,
			wantLaunched: true,
			checkPrompt: func(t *testing.T, prompt string) {
				if !strings.Contains(prompt, `"event":"push"`) {
					t.Errorf("prompt missing event: %q", prompt)
				}
			},
		},
		{
			name:         "big integer precision preserved",
			method:       http.MethodPost,
			path:         "/v1/invoke",
			contentType:  "application/json",
			token:        secret,
			body:         `{"id":1234567890123456789}`,
			wantStatus:   http.StatusAccepted,
			wantLaunched: true,
			checkPrompt: func(t *testing.T, prompt string) {
				if prompt != `{"id":1234567890123456789}` {
					t.Errorf("prompt not byte-for-byte: got %q, want %q", prompt, `{"id":1234567890123456789}`)
				}
			},
		},
		{
			name:         "key order preserved",
			method:       http.MethodPost,
			path:         "/v1/invoke",
			contentType:  "application/json",
			token:        secret,
			body:         `{"b":1,"a":2}`,
			wantStatus:   http.StatusAccepted,
			wantLaunched: true,
			checkPrompt: func(t *testing.T, prompt string) {
				bIdx := strings.Index(prompt, `"b"`)
				aIdx := strings.Index(prompt, `"a"`)
				if bIdx < 0 || aIdx < 0 {
					t.Fatalf("prompt missing keys: %q", prompt)
				}
				if bIdx >= aIdx {
					t.Errorf(`"b" should precede "a" in prompt, got %q`, prompt)
				}
			},
		},
		{
			name:        "missing token is 401",
			method:      http.MethodPost,
			path:        "/v1/invoke",
			contentType: "application/json",
			token:       "",
			body:        `{}`,
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "wrong token is 401",
			method:      http.MethodPost,
			path:        "/v1/invoke",
			contentType: "application/json",
			token:       "nope",
			body:        `{}`,
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "wrong content type is 415",
			method:      http.MethodPost,
			path:        "/v1/invoke",
			contentType: "text/plain",
			token:       secret,
			body:        "hello",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "invalid json is 400",
			method:      http.MethodPost,
			path:        "/v1/invoke",
			contentType: "application/json",
			token:       secret,
			body:        `not json`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:   "healthz no auth needed",
			method: http.MethodGet,
			path:   "/healthz",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			launcher, h := newTestRouter(t, secret, 1<<20)

			var body io.Reader
			if tt.body != "" {
				body = bytes.NewBufferString(tt.body)
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.token != "" {
				req.Header.Set("X-Mithras-Token", tt.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			calls := launcher.Calls()
			if tt.wantLaunched && len(calls) != 1 {
				t.Fatalf("expected 1 launch, got %d", len(calls))
			}
			if !tt.wantLaunched && len(calls) != 0 {
				t.Fatalf("expected 0 launches, got %d", len(calls))
			}
			if tt.wantLaunched {
				var resp InvokeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp.RequestID == "" {
					t.Error("response RequestID is empty")
				}
				if resp.RequestID != calls[0].requestID {
					t.Errorf("response RequestID %q != launch ID %q", resp.RequestID, calls[0].requestID)
				}
				if tt.checkPrompt != nil {
					tt.checkPrompt(t, calls[0].prompt)
				}
			}
		})
	}
}

func TestInvokeHandler_bodyTooLarge(t *testing.T) {
	t.Parallel()

	launcher, h := newTestRouter(t, "x", 16)
	big := strings.Repeat("a", 64)

	req := httptest.NewRequest(http.MethodPost, "/v1/invoke",
		bytes.NewBufferString(`{"pad":"`+big+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mithras-Token", "x")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if n := len(launcher.Calls()); n != 0 {
		t.Errorf("launch count = %d, want 0", n)
	}
}
