package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
)

// ToolAdapter wraps a single MCP tool so it satisfies the agentloop.Tool
// interface. It holds a reference to the owning session; the caller is
// responsible for keeping that session alive while the adapter is in use.
type ToolAdapter struct {
	serverName string
	toolName   string
	tool       *mcpsdk.Tool
	session    *mcpsdk.ClientSession
}

// Name returns the namespaced tool name the agent loop sees.
func (a *ToolAdapter) Name() string {
	return ToolName(a.serverName, a.toolName)
}

// Usage converts the MCP input schema into an OpenAI function definition.
func (a *ToolAdapter) Usage() openai.FunctionDefinitionParam {
	params := schemaAsFunctionParameters(a.tool.InputSchema)
	return openai.FunctionDefinitionParam{
		Name:        a.Name(),
		Description: openai.String(a.tool.Description),
		Parameters:  params,
	}
}

// Valid checks that data parses as a JSON object. The MCP server does its own
// schema validation on [ClientSession.CallTool].
func (a *ToolAdapter) Valid(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("mcp: invalid arguments for %q: %w", a.Name(), err)
	}
	return nil
}

// Run calls the underlying MCP tool and returns the result as JSON so the
// agent loop can hand it back to the model. fs is ignored: MCP servers manage
// their own file access.
func (a *ToolAdapter) Run(ctx context.Context, _ fs.FS, data []byte) ([]byte, error) {
	var args map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &args); err != nil {
			return nil, fmt.Errorf("mcp: parsing arguments for %q: %w", a.Name(), err)
		}
	}

	res, err := a.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      a.toolName,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: calling %q: %w", a.Name(), err)
	}

	out, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal result of %q: %w", a.Name(), err)
	}
	return out, nil
}

// schemaAsFunctionParameters normalizes the MCP input-schema representation
// (which comes over the wire as map[string]any, but may also be json.RawMessage
// or a typed value) into [openai.FunctionParameters], which is itself a
// map[string]any.
func schemaAsFunctionParameters(schema any) openai.FunctionParameters {
	switch s := schema.(type) {
	case nil:
		return openai.FunctionParameters{"type": "object"}
	case openai.FunctionParameters:
		return s
	case map[string]any:
		return openai.FunctionParameters(s)
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(s, &m); err == nil {
			return openai.FunctionParameters(m)
		}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return openai.FunctionParameters{"type": "object"}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return openai.FunctionParameters{"type": "object"}
	}
	return openai.FunctionParameters(m)
}
