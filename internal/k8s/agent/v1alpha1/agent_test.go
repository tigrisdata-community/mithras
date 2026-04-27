package agentv1alpha1

import (
	"errors"
	"testing"
)

func TestAgentIngressValid(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		input   AgentIngress
		wantErr []error
	}{
		{
			name:  "happy path",
			input: AgentIngress{Host: "agent.example.com", Class: "nginx"},
		},
		{
			name:    "missing host",
			input:   AgentIngress{Class: "nginx"},
			wantErr: []error{ErrBadSpec, ErrMissingField},
		},
		{
			name:    "missing class",
			input:   AgentIngress{Host: "agent.example.com"},
			wantErr: []error{ErrBadSpec, ErrMissingField},
		},
		{
			name:    "missing host and class",
			input:   AgentIngress{},
			wantErr: []error{ErrBadSpec, ErrMissingField},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.input.Valid()

			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("got unexpected error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("want error, got nil")
			}

			for _, want := range tt.wantErr {
				if !errors.Is(err, want) {
					t.Logf("want: %v", want)
					t.Logf("got:  %v", err)
					t.Errorf("error chain missing sentinel %v", want)
				}
			}
		})
	}
}

func TestAgentSpecValidIngress(t *testing.T) {
	t.Parallel()

	base := AgentSpec{
		Model:             "claude-opus-4-7",
		CredsSecret:       "anthropic-creds",
		SystemPrompt:      "be helpful",
		Bucket:            "agent-bucket",
		PerRequestTimeout: "30s",
	}

	for _, tt := range []struct {
		name    string
		ingress *AgentIngress
		wantErr []error
	}{
		{
			name:    "valid ingress",
			ingress: &AgentIngress{Host: "agent.example.com", Class: "nginx"},
		},
		{
			name:    "invalid ingress propagates",
			ingress: &AgentIngress{},
			wantErr: []error{ErrBadSpec, ErrMissingField},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec := base
			spec.Ingress = tt.ingress

			err := spec.Valid()

			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("got unexpected error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("want error, got nil")
			}

			for _, want := range tt.wantErr {
				if !errors.Is(err, want) {
					t.Logf("want: %v", want)
					t.Logf("got:  %v", err)
					t.Errorf("error chain missing sentinel %v", want)
				}
			}
		})
	}
}
