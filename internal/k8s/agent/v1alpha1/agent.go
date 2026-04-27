package agentv1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	APIVersion = "mithras.tigris.sh/v1alpha1"
	KindAgent  = "Agent"
)

var (
	ErrBadSpec       = errors.New("agentv1alpha1: spec settings invalid")
	ErrMissingField  = errors.New("agentv1alpha1: missing configuration field")
	ErrInvalidFormat = errors.New("agentv1alpha1: format invalid")
)

type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentSpec `json:"spec"`
}

// MarshalJSON sets magic Kubernetes Values and marshals the Agent as k8s-ready JSON.
func (a Agent) MarshalJSON() ([]byte, error) {
	a.Kind = KindAgent
	a.APIVersion = APIVersion

	// separate type alias here to avoid infinite recursion and reflect on the struct directly
	type AgentAlt Agent
	return json.Marshal(AgentAlt(a))
}

// UnmarshalJSON unmarshals the Agent definition and validates the configuration.
func (a *Agent) UnmarshalJSON(data []byte) error {
	// separate type alias here to avoid infinite recursion and reflect on the struct directly
	type AgentAlt Agent
	if err := json.Unmarshal(data, (*AgentAlt)(a)); err != nil {
		return err
	}

	if a.APIVersion != APIVersion {
		return fmt.Errorf("unexpected api version: expected %s but got %s", APIVersion, a.APIVersion)
	}

	if a.Kind != KindAgent {
		return fmt.Errorf("unexpected kind: expected %s but got %s", KindAgent, a.Kind)
	}

	if err := a.Valid(); err != nil {
		return fmt.Errorf("can't validate agent spec: %w", err)
	}

	return nil
}

func (a *Agent) Valid() error {
	return a.Spec.Valid()
}

type AgentSpec struct {
	Model             string   `json:"model"`
	CredsSecret       string   `json:"credsSecret"`
	SystemPrompt      string   `json:"systemPrompt"`
	Bucket            string   `json:"bucket"`
	Tools             []string `json:"tools"`
	PerRequestTimeout string   `json:"perRequestTimeout"`

	Ingress *AgentIngress `json:"ingress"`
}

// Valid ensures that agent settings are valid, returning errors if this is not the case.
func (as *AgentSpec) Valid() error {
	var errs []error

	if as.Model == "" {
		errs = append(errs, fmt.Errorf("%w: model", ErrMissingField))
	}

	if as.CredsSecret == "" {
		errs = append(errs, fmt.Errorf("%w: credsSecret", ErrMissingField))
	}

	if as.SystemPrompt == "" {
		errs = append(errs, fmt.Errorf("%w: systemPrompt", ErrMissingField))
	}

	if as.Bucket == "" {
		errs = append(errs, fmt.Errorf("%w: bucket", ErrMissingField))
	}

	if as.PerRequestTimeout == "" {
		errs = append(errs, fmt.Errorf("%w: perRequestTimeout", ErrMissingField))
	}

	if _, err := time.ParseDuration(as.PerRequestTimeout); err != nil {
		errs = append(errs, fmt.Errorf("%w: time.ParseDuration(perRequestTimeout: %q): %w", ErrInvalidFormat, as.PerRequestTimeout, err))
	}

	if as.Ingress == nil {
		errs = append(errs, fmt.Errorf("%w: ingress", ErrMissingField))
	}

	if as.Ingress != nil {
		if err := as.Ingress.Valid(); err != nil {
			errs = append(errs, fmt.Errorf("invalid agentIngress: %w", err))
		}
	}

	if len(errs) != 0 {
		return errors.Join(slices.Insert(errs, 0, ErrBadSpec)...)
	}

	return nil
}

type AgentIngress struct {
	Host  string `json:"host"`
	Class string `json:"class"`
}

// Valid ensures that ingress settings are valid, returning errors if this is not the case.
func (ai *AgentIngress) Valid() error {
	var errs []error

	if ai.Host == "" {
		errs = append(errs, fmt.Errorf("%w: host", ErrMissingField))
	}

	if ai.Class == "" {
		errs = append(errs, fmt.Errorf("%w: class", ErrMissingField))
	}

	if len(errs) != 0 {
		return errors.Join(slices.Insert(errs, 0, ErrBadSpec)...)
	}

	return nil
}
