package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/openai/openai-go/v3"
	"github.com/tigrisdata/storage-go"
	"github.com/tigrisdata/storage-go/tigrisheaders"
	"tangled.org/xeiaso.net/kefka/s3fs"

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
// call needs. It is built once at startup and shared across goroutines. The
// per-request filesystem is materialized inside [AgentRunner.Run] by forking
// SourceBucket so the agent gets an isolated copy of the bucket that is
// disposed of when the invocation finishes.
type RunnerDeps struct {
	AgentName         string
	Model             string
	SystemPrompt      string
	Client            openai.Client
	Storage           *storage.Client
	SourceBucket      string
	Tools             []agentloop.Tool
	Logger            *slog.Logger
	PerRequestTimeout time.Duration
	ParallelToolCalls bool
}

// AgentRunner is the production [Runner] that drives a real agentloop.
type AgentRunner struct {
	deps RunnerDeps
}

// NewAgentRunner constructs an [AgentRunner] from deps. It returns an error
// when SourceBucket or Storage are unset because both are required to
// materialize the per-request bucket fork inside [AgentRunner.Run].
func NewAgentRunner(deps RunnerDeps) (*AgentRunner, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	return &AgentRunner{deps: deps}, nil
}

// Run builds a fresh agentloop.Impl and invokes it with prompt. Before the
// agent runs, the runner forks the configured source bucket into a
// per-request fork named "<sourceBucket>-<requestID>" and mounts that fork
// as the agent's filesystem; the fork is force-deleted after Run returns so
// nothing persists between invocations. On success it logs the final
// message and token usage; on failure it logs the error.
func (r *AgentRunner) Run(ctx context.Context, requestID, prompt string) {
	lg := r.deps.Logger.With(
		"component", "webhook.runner",
		"requestID", requestID,
		"agent", r.deps.AgentName,
	)

	sessionBucket := r.deps.SourceBucket + "-" + requestID
	lg = lg.With("sessionBucket", sessionBucket, "sourceBucket", r.deps.SourceBucket)

	if _, err := r.deps.Storage.CreateBucketFork(ctx, r.deps.SourceBucket, sessionBucket); err != nil {
		lg.Error("can't fork source bucket", "err", err)
		return
	}
	defer r.deleteSessionBucket(lg, sessionBucket)

	fsys, err := s3fs.NewS3FS(r.deps.Storage.Client, sessionBucket)
	if err != nil {
		lg.Error("can't build s3fs over session bucket", "err", err)
		return
	}

	agent := agentloop.New(agentloop.Options{
		Name:         r.deps.AgentName,
		ID:           requestID,
		SystemPrompt: r.deps.SystemPrompt,
		Model:        r.deps.Model,
		Tools:        r.deps.Tools,
		Client:       r.deps.Client,
		Logger:       lg,
		FS:           fsys,
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

// deleteSessionBucket force-deletes the per-request fork. It runs the
// delete on a fresh background context with a short timeout so cleanup
// still happens when the request context has been cancelled (drain,
// timeout, etc.).
func (r *AgentRunner) deleteSessionBucket(lg *slog.Logger, bucket string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, err := r.deps.Storage.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	}, tigrisheaders.WithHeader("Tigris-Force-Delete", "true")); err != nil {
		lg.Error("can't delete session bucket", "err", err, "bucket", bucket)
		return
	}
	lg.Info("session bucket deleted", "bucket", bucket)
}

// ErrIncompleteRunnerDeps is returned from [NewAgentRunner] when the deps
// don't have everything needed to materialize per-request bucket forks.
var ErrIncompleteRunnerDeps = errors.New("webhook: RunnerDeps missing required field")

func (d RunnerDeps) validate() error {
	if d.SourceBucket == "" {
		return fmt.Errorf("%w: SourceBucket", ErrIncompleteRunnerDeps)
	}
	if d.Storage == nil {
		return fmt.Errorf("%w: Storage", ErrIncompleteRunnerDeps)
	}
	return nil
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
