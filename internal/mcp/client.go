// Package mcp adapts the Model Context Protocol Go SDK to the
// [agentloop.Tool] interface so MCP-provided tools can be registered on an
// agent loop alongside built-in Go tools.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tigrisdata-community/mithras/internal/agentloop"
)

// Transport identifiers accepted by [Connect]. They match the strings used in
// the webhookd ConfigMap.
const (
	TransportStdio          = "stdio"
	TransportStreamableHTTP = "streamable-http"
	TransportSSE            = "sse"
)

// ErrUnknownTransport indicates that a [ServerSpec] specified a transport that
// this package does not support.
var ErrUnknownTransport = errors.New("mcp: unknown transport")

// ServerSpec describes one MCP server to connect to.
type ServerSpec struct {
	// Name is a short identifier used to namespace tool names on the agent
	// loop. It must be unique across all servers registered in a single
	// process.
	Name string
	// Transport selects the wire protocol. See the Transport* constants.
	Transport string
	// URL is the endpoint for HTTP-based transports.
	URL string
	// Command is the argv for the stdio transport. Command[0] is resolved
	// through $PATH.
	Command []string
	// Env is an optional set of environment variables to hand to the stdio
	// child process. It supplements (does not replace) the parent env.
	Env map[string]string
}

// Client holds one live MCP session plus the tools it exposes.
type Client struct {
	spec    ServerSpec
	session *mcpsdk.ClientSession
	tools   []*mcpsdk.Tool
}

// Connect establishes a session with the configured MCP server and lists its
// tools once so the caller can register adapters on an agent loop.
func Connect(ctx context.Context, spec ServerSpec, logger *slog.Logger) (*Client, error) {
	transport, err := newTransport(spec)
	if err != nil {
		return nil, err
	}

	impl := &mcpsdk.Implementation{Name: "mithras-webhookd", Version: "0"}
	opts := &mcpsdk.ClientOptions{Logger: logger}

	cli := mcpsdk.NewClient(impl, opts)
	session, err := cli.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %q: %w", spec.Name, err)
	}

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("mcp: list tools for %q: %w", spec.Name, err)
	}

	return &Client{spec: spec, session: session, tools: list.Tools}, nil
}

// Tools returns the agent-loop adapters for every tool exposed by this MCP
// server. Names are prefixed with the server name so multiple servers can
// register concurrently without colliding.
func (c *Client) Tools() []agentloop.Tool {
	out := make([]agentloop.Tool, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, &ToolAdapter{
			serverName: c.spec.Name,
			toolName:   t.Name,
			tool:       t,
			session:    c.session,
		})
	}
	return out
}

// Close terminates the underlying session.
func (c *Client) Close() error {
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}

func newTransport(spec ServerSpec) (mcpsdk.Transport, error) {
	switch spec.Transport {
	case TransportStdio:
		if len(spec.Command) == 0 {
			return nil, fmt.Errorf("mcp: stdio transport for %q has empty command", spec.Name)
		}
		cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
		cmd.Env = append(os.Environ(), flattenEnv(spec.Env)...)
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	case TransportStreamableHTTP:
		if spec.URL == "" {
			return nil, fmt.Errorf("mcp: streamable-http transport for %q has empty url", spec.Name)
		}
		return &mcpsdk.StreamableClientTransport{Endpoint: spec.URL}, nil
	case TransportSSE:
		if spec.URL == "" {
			return nil, fmt.Errorf("mcp: sse transport for %q has empty url", spec.Name)
		}
		return &mcpsdk.SSEClientTransport{Endpoint: spec.URL}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownTransport, spec.Transport)
	}
}

func flattenEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Pool owns a collection of live MCP clients and hands out their tool adapters
// as a single flat slice for registration on an agent loop.
type Pool struct {
	mu      sync.Mutex
	clients []*Client
}

// NewPool connects to every server in specs and collects their tools. On any
// failure, any already-connected clients are closed before returning.
func NewPool(ctx context.Context, specs []ServerSpec, logger *slog.Logger) (*Pool, error) {
	p := &Pool{}
	for _, spec := range specs {
		cli, err := Connect(ctx, spec, logger)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.clients = append(p.clients, cli)
	}
	return p, nil
}

// Tools returns the flattened list of tool adapters from every connected
// server.
func (p *Pool) Tools() []agentloop.Tool {
	p.mu.Lock()
	defer p.mu.Unlock()

	var out []agentloop.Tool
	for _, cli := range p.clients {
		out = append(out, cli.Tools()...)
	}
	return out
}

// Close terminates every underlying session, returning a joined error for any
// that failed.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	for _, cli := range p.clients {
		if err := cli.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	p.clients = nil
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("mcp: closing pool:\n%w", errors.Join(errs...))
}

// ToolName returns the namespaced tool name for a server/tool pair. It is used
// by the adapter and exposed here so callers (and tests) can predict the name
// the agent loop will see.
func ToolName(server, tool string) string {
	// Sanitize the server name to fit the openai function-name charset. The
	// MCP spec does not restrict server/tool names, but OpenAI does.
	return sanitize(server) + "__" + sanitize(tool)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
