package webhookconfig

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()

	minimal := `
agentName: a
model: gpt-5
providerBaseURL: https://api.openai.com/v1
systemPrompt: |
  You are helpful.
bucket: b
tools:
  - python
`

	tests := []struct {
		name          string
		input         string
		wantErr       error
		wantErrSubstr string
		check         func(t *testing.T, cfg *Config)
	}{
		{
			name:  "minimal valid config",
			input: minimal,
			check: func(t *testing.T, cfg *Config) {
				if cfg.AgentName != "a" {
					t.Errorf("AgentName = %q, want %q", cfg.AgentName, "a")
				}
				if cfg.S3.EffectiveEndpoint() != "https://t3.storage.dev" {
					t.Errorf("default endpoint = %q, want tigris", cfg.S3.EffectiveEndpoint())
				}
				if !cfg.S3.EffectivePathStyle() {
					t.Error("default path style should be true")
				}
			},
		},
		{
			name: "missing required field",
			input: `
model: gpt-5
providerBaseURL: https://api.openai.com/v1
systemPrompt: x
bucket: b
`,
			wantErr: ErrMissingField,
		},
		{
			name: "stdio without command is rejected",
			input: minimal + `
mcpServers:
  - name: s
    transport: stdio
`,
			wantErr: ErrMissingField,
		},
		{
			name: "streamable-http without url is rejected",
			input: minimal + `
mcpServers:
  - name: s
    transport: streamable-http
`,
			wantErr: ErrMissingField,
		},
		{
			name: "unknown transport is rejected",
			input: minimal + `
mcpServers:
  - name: s
    transport: carrier-pigeon
    url: http://example.test
`,
			wantErr: ErrInvalidTransport,
		},
		{
			name: "duplicate server name is rejected",
			input: minimal + `
mcpServers:
  - name: s
    transport: streamable-http
    url: http://a.test
  - name: s
    transport: streamable-http
    url: http://b.test
`,
			wantErrSubstr: "duplicate mcp server name",
		},
		{
			name: "valid mixed transports",
			input: minimal + `
mcpServers:
  - name: http-srv
    transport: streamable-http
    url: http://a.test
  - name: sse-srv
    transport: sse
    url: http://b.test/sse
  - name: stdio-srv
    transport: stdio
    command: ["/bin/true"]
`,
			check: func(t *testing.T, cfg *Config) {
				if n := len(cfg.MCPServers); n != 3 {
					t.Errorf("len(MCPServers) = %d, want 3", n)
				}
			},
		},
		{
			name: "unknown field is rejected",
			input: minimal + `
mcp_servers:
  - name: oops
`,
			wantErrSubstr: "field mcp_servers not found",
		},
		{
			name: "negative perRequestTimeout is rejected",
			input: minimal + `
perRequestTimeout: -1s
`,
			wantErr: ErrInvalidValue,
		},
		{
			name: "zero perRequestTimeout is allowed",
			input: minimal + `
perRequestTimeout: 0s
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.PerRequestTimeout != 0 {
					t.Errorf("PerRequestTimeout = %v, want 0", cfg.PerRequestTimeout)
				}
			},
		},
		{
			name: "positive perRequestTimeout is parsed",
			input: minimal + `
perRequestTimeout: 30s
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.PerRequestTimeout != 30*time.Second {
					t.Errorf("PerRequestTimeout = %v, want 30s", cfg.PerRequestTimeout)
				}
			},
		},
		{
			name:  "parallelToolCalls defaults to true when unset",
			input: minimal,
			check: func(t *testing.T, cfg *Config) {
				if cfg.ParallelToolCalls != nil {
					t.Errorf("ParallelToolCalls = %v, want nil", cfg.ParallelToolCalls)
				}
				if !cfg.EffectiveParallelToolCalls() {
					t.Error("EffectiveParallelToolCalls() = false, want true when unset")
				}
			},
		},
		{
			name: "parallelToolCalls explicit false is honored",
			input: minimal + `
parallelToolCalls: false
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.ParallelToolCalls == nil || *cfg.ParallelToolCalls != false {
					t.Errorf("ParallelToolCalls = %v, want &false", cfg.ParallelToolCalls)
				}
				if cfg.EffectiveParallelToolCalls() {
					t.Error("EffectiveParallelToolCalls() = true, want false when set to false")
				}
			},
		},
		{
			name: "parallelToolCalls explicit true is honored",
			input: minimal + `
parallelToolCalls: true
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.ParallelToolCalls == nil || *cfg.ParallelToolCalls != true {
					t.Errorf("ParallelToolCalls = %v, want &true", cfg.ParallelToolCalls)
				}
				if !cfg.EffectiveParallelToolCalls() {
					t.Error("EffectiveParallelToolCalls() = false, want true when set to true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Parse([]byte(tt.input))
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case tt.wantErrSubstr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("err = %v, want to contain %q", err, tt.wantErrSubstr)
				}
				return
			case err != nil:
				t.Fatalf("unexpected err: %v", err)
			}

			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestExpandEnvMap(t *testing.T) {
	t.Setenv("WEBHOOK_TEST_TOKEN", "sekret")

	got := expandEnvMap(map[string]string{
		"FOO":     "${WEBHOOK_TEST_TOKEN}",
		"BAR":     "prefix-${WEBHOOK_TEST_TOKEN}-suffix",
		"MISSING": "${WEBHOOK_TEST_UNSET}",
		"PLAIN":   "nothing-to-expand",
	})

	want := map[string]string{
		"FOO":     "sekret",
		"BAR":     "prefix-sekret-suffix",
		"MISSING": "",
		"PLAIN":   "nothing-to-expand",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
