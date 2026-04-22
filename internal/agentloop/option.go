package agentloop

import "github.com/openai/openai-go/v3"

// EnableParallelToolCalling is an option for [Impl.Run] that permits the
// model to request multiple tool calls in a single assistant turn.
func EnableParallelToolCalling(params *openai.ChatCompletionNewParams) {
	params.ParallelToolCalls = openai.Bool(true)
}
