// Package webhook implements the HTTP-facing pieces of the webhookd binary:
// config loading, middleware, request handling, and per-request agent wiring.
package webhook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/tigrisdata-community/mithras/internal/mcp"
	"gopkg.in/yaml.v3"
)

var (
	ErrInvalidTransport = errors.New("webhook: mcp server has unknown transport")
	ErrMissingField     = errors.New("webhook: required field is missing")
	ErrInvalidValue     = errors.New("webhook: invalid configuration value")
	ErrUnknownTool      = errors.New("webhook: tool not in built-in registry")
)

// Config is the on-disk shape of the ConfigMap.
type Config struct {
	AgentName         string        `yaml:"agentName"`
	Model             string        `yaml:"model"`
	ProviderBaseURL   string        `yaml:"providerBaseURL"`
	SystemPrompt      string        `yaml:"systemPrompt"`
	Bucket            string        `yaml:"bucket"`
	S3                S3Config      `yaml:"s3"`
	Tools             []string      `yaml:"tools"`
	MCPServers        []MCPServer   `yaml:"mcpServers"`
	PerRequestTimeout time.Duration `yaml:"perRequestTimeout"`
	ParallelToolCalls *bool         `yaml:"parallelToolCalls"`
}

// EffectiveParallelToolCalls returns the configured value of ParallelToolCalls,
// defaulting to true when the field is unset. A pointer lets callers
// distinguish "unset" from "explicitly false".
func (c *Config) EffectiveParallelToolCalls() bool {
	if c.ParallelToolCalls == nil {
		return true
	}
	return *c.ParallelToolCalls
}

// S3Config controls how the S3 client is built. All fields are optional; the
// defaults point at Tigris.
type S3Config struct {
	Endpoint     string `yaml:"endpoint"`
	Region       string `yaml:"region"`
	UsePathStyle *bool  `yaml:"usePathStyle"`
}

// EffectiveEndpoint returns S3.Endpoint, falling back to the Tigris default.
func (s S3Config) EffectiveEndpoint() string {
	if s.Endpoint == "" {
		return "https://t3.storage.dev"
	}
	return s.Endpoint
}

// EffectiveRegion returns S3.Region, falling back to "auto".
func (s S3Config) EffectiveRegion() string {
	if s.Region == "" {
		return "auto"
	}
	return s.Region
}

// EffectivePathStyle returns the configured UsePathStyle or true (the Tigris
// default) when unset.
func (s S3Config) EffectivePathStyle() bool {
	if s.UsePathStyle == nil {
		return true
	}
	return *s.UsePathStyle
}

// MCPServer describes one upstream MCP server the agent can call into.
type MCPServer struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"`
	URL       string            `yaml:"url"`
	Command   []string          `yaml:"command"`
	Env       map[string]string `yaml:"env"`
}

// LoadConfig reads and validates the ConfigMap file at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("webhook: reading config: %w", err)
	}

	return ParseConfig(raw)
}

// ParseConfig parses and validates a raw YAML document. It performs env-var
// expansion on MCP server env values before returning. Unknown fields are
// rejected so typos surface as errors rather than silent no-ops.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("webhook: parsing config: %w", err)
	}

	for i := range cfg.MCPServers {
		cfg.MCPServers[i].Env = expandEnvMap(cfg.MCPServers[i].Env)
	}

	if err := cfg.Valid(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Valid checks that all required top-level fields are set and that each MCP
// server's transport-specific fields are populated correctly.
func (c *Config) Valid() error {
	var errs []error

	if c.AgentName == "" {
		errs = append(errs, fmt.Errorf("%w: agentName", ErrMissingField))
	}
	if c.Model == "" {
		errs = append(errs, fmt.Errorf("%w: model", ErrMissingField))
	}
	if c.ProviderBaseURL == "" {
		errs = append(errs, fmt.Errorf("%w: providerBaseURL", ErrMissingField))
	}
	if c.SystemPrompt == "" {
		errs = append(errs, fmt.Errorf("%w: systemPrompt", ErrMissingField))
	}
	if c.Bucket == "" {
		errs = append(errs, fmt.Errorf("%w: bucket", ErrMissingField))
	}

	names := map[string]struct{}{}
	for i, srv := range c.MCPServers {
		if srv.Name == "" {
			errs = append(errs, fmt.Errorf("%w: mcpServers[%d].name", ErrMissingField, i))
			continue
		}
		if _, dup := names[srv.Name]; dup {
			errs = append(errs, fmt.Errorf("webhook: duplicate mcp server name %q", srv.Name))
			continue
		}
		names[srv.Name] = struct{}{}

		switch srv.Transport {
		case mcp.TransportStdio:
			if len(srv.Command) == 0 {
				errs = append(errs, fmt.Errorf("%w: mcpServers[%q].command (required for stdio)", ErrMissingField, srv.Name))
			}
		case mcp.TransportStreamableHTTP, mcp.TransportSSE:
			if srv.URL == "" {
				errs = append(errs, fmt.Errorf("%w: mcpServers[%q].url (required for %s)", ErrMissingField, srv.Name, srv.Transport))
			}
		default:
			errs = append(errs, fmt.Errorf("%w: mcpServers[%q].transport=%q", ErrInvalidTransport, srv.Name, srv.Transport))
		}
	}

	if c.PerRequestTimeout < 0 {
		errs = append(errs, fmt.Errorf("%w: perRequestTimeout=%s (must be >= 0)", ErrInvalidValue, c.PerRequestTimeout))
	}

	if len(errs) != 0 {
		return fmt.Errorf("webhook: invalid config:\n%w", errors.Join(errs...))
	}
	return nil
}

// envVarPattern matches ${NAME} sequences where NAME is an env-var identifier.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnvMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = envVarPattern.ReplaceAllStringFunc(v, func(match string) string {
			name := match[2 : len(match)-1]
			return os.Getenv(name)
		})
	}
	return out
}
