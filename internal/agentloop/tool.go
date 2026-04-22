package agentloop

import (
	"context"
	"io/fs"

	"github.com/openai/openai-go/v3"
)

// Tool is a function the model may call during the agent loop. Each tool
// describes itself to the model via [Tool.Usage], validates raw JSON
// arguments via [Tool.Valid], and executes via [Tool.Run].
//
// Tools may return [ErrSentinelOkay] or [ErrSentinelAbort] from Run to stop
// the agent loop immediately. Any other error is reported back to the model
// so it can decide how to recover.
type Tool interface {
	// Name returns the tool's identifier. It must be unique within an
	// [Impl] and stable across calls.
	Name() string
	// Usage returns the function schema advertised to the model.
	Usage() openai.FunctionDefinitionParam
	// Valid checks that data (the raw JSON argument blob produced by the
	// model) is well-formed for this tool. A non-nil error is surfaced to
	// the model so it can retry with corrected arguments.
	Valid(data []byte) (err error)
	// Run executes the tool with the validated argument blob and returns
	// the raw bytes to hand back to the model as the tool message. The
	// fs argument is the filesystem configured on the owning [Impl] and
	// may be nil if no filesystem was supplied.
	Run(ctx context.Context, fs fs.FS, data []byte) ([]byte, error)
}
