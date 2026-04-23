package webhook

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/tigrisdata-community/mithras/internal/agentloop"
)

// Runner drives one agentloop invocation per call. It builds a fresh
// [agentloop.Impl] for each request so conversation histories do not bleed
// between webhooks.
type Runner interface {
	// Run executes the agent loop for a single webhook payload. It is
	// expected to log the final result itself; the caller has already
	// returned 202 to the webhook sender by the time Run is invoked.
	Run(ctx context.Context, requestID, prompt string)
}

// RunnerDeps bundles the process-wide state that every request-scoped runner
// call needs. It is built once at startup and shared across goroutines.
type RunnerDeps struct {
	AgentName         string
	Model             string
	SystemPrompt      string
	Client            openai.Client
	FS                fs.FS
	Tools             []agentloop.Tool
	Logger            *slog.Logger
	PerRequestTimeout time.Duration
	ParallelToolCalls bool
}

// AgentRunner is the production [Runner] that drives a real agentloop.
type AgentRunner struct {
	deps RunnerDeps
}

// NewAgentRunner constructs an [AgentRunner] from deps.
func NewAgentRunner(deps RunnerDeps) *AgentRunner {
	return &AgentRunner{deps: deps}
}

// Run builds a fresh agentloop.Impl and invokes it with prompt. On success it
// logs the final message and token usage; on failure it logs the error.
func (r *AgentRunner) Run(ctx context.Context, requestID, prompt string) {
	lg := r.deps.Logger.With(
		"component", "webhook.runner",
		"requestID", requestID,
		"agent", r.deps.AgentName,
	)

	agent := agentloop.New(agentloop.Options{
		Name:         r.deps.AgentName,
		ID:           requestID,
		SystemPrompt: r.deps.SystemPrompt,
		Model:        r.deps.Model,
		Tools:        r.deps.Tools,
		Client:       r.deps.Client,
		Logger:       lg,
		FS:           r.deps.FS,
	})

	if r.deps.PerRequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.deps.PerRequestTimeout)
		defer cancel()
	}

	var opts []func(*openai.ChatCompletionNewParams)
	if r.deps.ParallelToolCalls {
		opts = append(opts, agentloop.EnableParallelToolCalling)
	}

	result, err := agent.Run(ctx, prompt, opts...)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			lg.Error("agent timed out", "timeout", r.deps.PerRequestTimeout, "err", err)
			return
		}
		lg.Error("agent failed", "err", err)
		return
	}

	lg.Info("agent completed",
		"response", result.Response,
		"promptTokens", result.PromptTokens,
		"cachedPromptTokens", result.PromptCachedTokens,
		"completionTokens", result.CompletionTokens,
		"reasoningTokens", result.CompletionReasoningTokens,
	)
}

// BackgroundLauncher wraps a [Runner] and a [sync.WaitGroup] so the webhook
// handler can fire-and-forget without leaking goroutines on shutdown.
type BackgroundLauncher struct {
	runner Runner
	wg     *sync.WaitGroup
	root   context.Context
}

// NewBackgroundLauncher returns a launcher that spawns runner calls in
// goroutines tracked by wg. Each spawned goroutine inherits root (not the
// request context) so agent work survives the HTTP response being written.
func NewBackgroundLauncher(root context.Context, runner Runner, wg *sync.WaitGroup) *BackgroundLauncher {
	return &BackgroundLauncher{runner: runner, wg: wg, root: root}
}

// Launch starts a goroutine tracked by the wait group that invokes the
// wrapped runner with the process-wide root context.
func (l *BackgroundLauncher) Launch(requestID, prompt string) {
	l.wg.Go(func() {
		l.runner.Run(l.root, requestID, prompt)
	})
}
