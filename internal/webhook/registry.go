package webhook

import (
	"fmt"

	"github.com/tigrisdata-community/mithras/internal/agentloop"
	pythontool "github.com/tigrisdata-community/mithras/internal/tools/python"
)

// BuiltinTools returns a fresh map of the tools that ship with mithras, keyed
// by the name the model sees. The map is newly allocated per call so callers
// may mutate it freely.
func BuiltinTools() map[string]agentloop.Tool {
	return map[string]agentloop.Tool{
		"python": pythontool.Impl{},
	}
}

// SelectBuiltins returns the tools whose names are in want, preserving the
// order of want. It errors if any named tool isn't in the built-in registry.
func SelectBuiltins(want []string) ([]agentloop.Tool, error) {
	registry := BuiltinTools()
	out := make([]agentloop.Tool, 0, len(want))
	for _, name := range want {
		tool, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownTool, name)
		}
		out = append(out, tool)
	}
	return out, nil
}
