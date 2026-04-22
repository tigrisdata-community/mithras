// Package agentloop drives a chat-completion model through a tool-calling loop
// until the model produces a final answer or a tool signals that the loop
// should terminate.
//
// A caller builds an [Impl] with [New], then invokes [Impl.Run] with a user
// prompt. The loop sends the conversation to the model, dispatches any tool
// calls the model requests, appends tool results to the conversation, and
// repeats until the model stops or a sentinel error is returned.
package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ErrSentinelAbort is returned by [Impl.Run] when a tool signals that the
	// loop should stop with an unsuccessful status. Tools return this error
	// (or one wrapping it) to force the agent loop to bail out.
	ErrSentinelAbort = errors.New("agentloop: tool requested the agent loop to abort")

	// ErrSentinelOkay is returned by [Impl.Run] when a tool signals that the
	// loop has reached a successful terminal state and should stop. Tools
	// return this error (or one wrapping it) to end the loop cleanly.
	ErrSentinelOkay = errors.New("agentloop: tool requested the agent loop to stop (status okay)")

	tokensUsed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "mithras",
		Subsystem: "agentloop",
		Name:      "tokens_used",
	}, []string{"model", "kind"})
)

// Impl is a single agent instance that owns a conversation with a chat model
// and the tools the model may invoke. It maintains the running message
// history across calls to [Impl.Run], so subsequent prompts continue the same
// conversation.
//
// Impl is safe for concurrent use: [Impl.Run] serializes calls through an
// internal mutex.
type Impl struct {
	// Name is a human-readable label for this agent, used in logs.
	Name string
	// ID uniquely identifies this agent instance. If not supplied to [New]
	// it is generated as a UUIDv7.
	ID string
	// Tools is the set of tools the model may call, keyed by tool name.
	Tools map[string]Tool
	// SystemPrompt is the system message seeded at the head of the
	// conversation.
	SystemPrompt string

	model string
	cli   openai.Client
	lg    *slog.Logger
	fs    fs.FS

	messages []openai.ChatCompletionMessageParamUnion
	lock     sync.Mutex
}

// Options configures a new [Impl] constructed by [New].
type Options struct {
	// Name is a human-readable label for the agent, used in logs.
	Name string
	// ID uniquely identifies the agent instance. If empty, [New] generates
	// a UUIDv7.
	ID string
	// SystemPrompt is the system message seeded at the start of the
	// conversation.
	SystemPrompt string
	// Model is the chat-completion model identifier (e.g. "gpt-4o").
	Model string
	// Tools is the list of tools available to the model. Duplicate names
	// are resolved by last-write-wins when stored in [Impl.Tools].
	Tools []Tool
	// Client is the OpenAI-compatible client used to issue completions.
	Client openai.Client
	// Logger receives structured log output from the loop.
	Logger *slog.Logger
	// FS is an optional filesystem made available to tools that need
	// read-only file access.
	FS fs.FS
}

// New constructs an [Impl] from opts. If opts.ID is empty, a UUIDv7 is
// generated. The system prompt is seeded as the first message in the
// conversation.
func New(opts Options) *Impl {
	if opts.ID == "" {
		opts.ID = uuid.Must(uuid.NewV7()).String()
	}

	toolMap := map[string]Tool{}
	for _, tool := range opts.Tools {
		toolMap[tool.Name()] = tool
	}

	result := Impl{
		Name:         opts.Name,
		ID:           opts.ID,
		Tools:        toolMap,
		SystemPrompt: opts.SystemPrompt,
		model:        opts.Model,
		cli:          opts.Client,
		lg:           opts.Logger,
		fs:           opts.FS,
		messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(opts.SystemPrompt),
		},
	}

	return &result
}

// Result is returned by [Impl.Run] and summarizes the outcome of a single
// invocation of the agent loop.
type Result struct {
	// Messages is the full conversation history at the point the loop
	// returned, including the model's responses and any tool results.
	Messages []openai.ChatCompletionMessageParamUnion
	// Response is the content of the model's final assistant message, if
	// any.
	Response string

	// PromptTokens is the total number of prompt tokens consumed across
	// all completions made during the run.
	PromptTokens int64
	// PromptCachedTokens is the subset of prompt tokens served from the
	// provider's prompt cache.
	PromptCachedTokens int64
	// CompletionTokens is the total number of completion tokens produced
	// across all completions made during the run.
	CompletionTokens int64
	// CompletionReasoningTokens is the subset of completion tokens spent
	// on reasoning (for reasoning-capable models).
	CompletionReasoningTokens int64
}

// Run appends prompt to the conversation as a user message and drives the
// agent loop until the model emits a terminal response, a tool returns
// [ErrSentinelOkay] or [ErrSentinelAbort], or ctx is cancelled.
//
// Each opt is applied to the [openai.ChatCompletionNewParams] used for every
// completion request, allowing callers to tune parameters such as
// temperature or parallel tool calls (see [EnableParallelToolCalling]).
//
// Run retries transient completion failures up to five times with linear
// backoff. Run is safe to call concurrently; calls are serialized.
//
// The returned [Result] reflects progress even when an error is returned, so
// callers may inspect partial token usage and the message history on
// failure.
func (i *Impl) Run(ctx context.Context, prompt string, opts ...func(*openai.ChatCompletionNewParams)) (*Result, error) {
	i.lock.Lock()
	defer i.lock.Unlock()

	lg := i.lg.With("component", "agentloop", "name", i.Name, "id", i.ID, "model", i.model)

	i.messages = append(i.messages, openai.UserMessage(prompt))

	failCount := 0
	const failMax = 5

	result := Result{}

	for {
		select {
		case <-ctx.Done():
			lg.Error("context done", "err", ctx.Err())
			return &result, ctx.Err()
		default:
		}

		params := openai.ChatCompletionNewParams{
			Messages: i.messages,
			Model:    openai.ChatModel(i.model),
		}

		for _, opt := range opts {
			opt(&params)
		}

		for _, tool := range i.Tools {
			params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(tool.Usage()))
		}

		completion, err := i.cli.Chat.Completions.New(ctx, params)
		if err != nil {
			failCount++

			if failCount == failMax {
				return &result, fmt.Errorf("can't reach remote API: %w", err)
			}

			lg.Error("can't get completion, sleeping and retrying", "err", err, "failCount", failCount, "failMax", failMax)
			time.Sleep(time.Duration(failCount) * time.Second)
			continue
		}

		tokensUsed.WithLabelValues(i.model, "input").Add(float64(completion.Usage.PromptTokens))
		tokensUsed.WithLabelValues(i.model, "output").Add(float64(completion.Usage.CompletionTokens))
		tokensUsed.WithLabelValues(i.model, "cached").Add(float64(completion.Usage.PromptTokensDetails.CachedTokens))
		tokensUsed.WithLabelValues(i.model, "reasoning").Add(float64(completion.Usage.CompletionTokensDetails.ReasoningTokens))

		result.PromptTokens += completion.Usage.PromptTokens
		result.PromptCachedTokens += completion.Usage.PromptTokensDetails.CachedTokens
		result.CompletionTokens += completion.Usage.CompletionTokens
		result.CompletionReasoningTokens += completion.Usage.CompletionTokensDetails.ReasoningTokens

		choice := completion.Choices[0]
		resp := choice.Message

		i.messages = append(i.messages, resp.ToParam())
		result.Messages = i.messages

		if resp.Content != "" {
			result.Response = resp.Content
		}

		lg.Debug("got finish reason", "reason", choice.FinishReason)
		if choice.FinishReason == "stop" {
			return &result, nil
		}

		toolCalls := completion.Choices[0].Message.ToolCalls

		for _, tc := range toolCalls {
			lg := lg.With("tool", tc.Function.Name, "toolcall_id", tc.ID)
			tool, ok := i.Tools[tc.Function.Name]
			if !ok {
				lg.Error("AI model chose tool that did not exist, asking it to try again")
				i.messages = append(i.messages, openai.UserMessage(fmt.Sprintf("Tool %q does not exist, please try again.", tc.Function.Name)))
				continue
			}

			args := []byte(tc.Function.Arguments)
			if err := tool.Valid(args); err != nil {
				lg.Error("AI model produced invalid arguments", "err", err)
				i.messages = append(i.messages, openai.UserMessage(fmt.Sprintf("When calling tool %q, you got an argument validation error: %v", tool.Name(), err)))
				continue
			}

			lg.Debug("calling tool", "args", json.RawMessage(args))

			toolResult, err := tool.Run(ctx, i.fs, args)
			if err != nil {
				switch {
				case errors.Is(err, ErrSentinelOkay):
					lg.Info("tool requested happy exit", "err", err)
					return &result, err
				case errors.Is(err, ErrSentinelAbort):
					lg.Info("tool requested unhappy abort", "err", err)
					return &result, err
				default:
					lg.Error("failed to run tool", "err", err)
					i.messages = append(i.messages, openai.ToolMessage(fmt.Sprintf("internal error when running tool %q: %v", tool.Name(), err), tc.ID))
					continue
				}
			}

			lg.Debug("got response", "result", string(toolResult))

			i.messages = append(i.messages, openai.ToolMessage(string(toolResult), tc.ID))
		}
	}
}
