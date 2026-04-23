package mcp

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestToolName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		server string
		tool   string
		want   string
	}{
		{name: "plain", server: "fs", tool: "read", want: "fs__read"},
		{name: "sanitizes dots", server: "my.server", tool: "do", want: "my_server__do"},
		{name: "sanitizes spaces", server: "my server", tool: "do it", want: "my_server__do_it"},
		{name: "preserves dashes", server: "fs-1", tool: "read-file", want: "fs-1__read-file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ToolName(tt.server, tt.tool)
			if got != tt.want {
				t.Errorf("ToolName(%q, %q) = %q, want %q", tt.server, tt.tool, got, tt.want)
			}
		})
	}
}

func TestSchemaAsFunctionParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input any
		want  openai.FunctionParameters
	}{
		{
			name: "nil schema yields object placeholder",
			input: nil,
			want:  openai.FunctionParameters{"type": "object"},
		},
		{
			name: "map passthrough",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string"},
				},
			},
			want: openai.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string"},
				},
			},
		},
		{
			name:  "raw json schema",
			input: json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`),
			want: openai.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"x": map[string]any{"type": "number"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := schemaAsFunctionParameters(tt.input)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("got %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestToolAdapterValid(t *testing.T) {
	t.Parallel()

	a := &ToolAdapter{serverName: "s", toolName: "t"}

	tests := []struct {
		name    string
		input   []byte
		wantErr bool
	}{
		{name: "empty is ok", input: nil, wantErr: false},
		{name: "valid object", input: []byte(`{"x":1}`), wantErr: false},
		{name: "garbage fails", input: []byte(`not json`), wantErr: true},
		{name: "array is not object", input: []byte(`[1,2]`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := a.Valid(tt.input)
			if tt.wantErr && err == nil {
				t.Error("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
		})
	}
}
