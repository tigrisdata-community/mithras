// Command webhookd is the HTTP front-end for a mithras agent. Configuration
// (system prompt, model, tools, bucket, MCP servers) is loaded from a mounted
// Kubernetes ConfigMap; secrets come from the process environment. Each
// incoming POST /v1/invoke launches a fresh agent loop in the background and
// the final response is logged when the agent stops.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/facebookgo/flagenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tigrisdata-community/mithras/internal/agentloop"
	mcpclient "github.com/tigrisdata-community/mithras/internal/mcp"
	"github.com/tigrisdata-community/mithras/internal/s3fs"
	"github.com/tigrisdata-community/mithras/internal/webhook"
	"github.com/tigrisdata-community/mithras/internal/webhook/webhookconfig"
)

var (
	configPath    = flag.String("config-path", "/etc/mithras/config.yaml", "path to ConfigMap-mounted config file")
	bind          = flag.String("bind", ":8080", "HTTP bind address")
	slogLevel     = flag.String("slog-level", "info", "slog level (debug, info, warn, error)")
	drainTimeout  = flag.Duration("drain-timeout", 60*time.Second, "maximum wait for in-flight agents on shutdown")
	maxBodyBytes  = flag.Int64("max-body-bytes", 1<<20, "maximum accepted webhook body size in bytes")
	metricsEnable = flag.Bool("metrics", true, "expose /metrics")
)

func main() {
	flagenv.Parse()
	flag.Parse()

	lg := newLogger(*slogLevel)
	slog.SetDefault(lg)

	if err := run(context.Background(), lg); err != nil {
		lg.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, lg *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := webhookconfig.Load(*configPath)
	if err != nil {
		return err
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return errors.New("OPENAI_API_KEY not set")
	}
	secret := os.Getenv("WEBHOOK_SHARED_SECRET")
	if secret == "" {
		return errors.New("WEBHOOK_SHARED_SECRET not set")
	}

	openaiCli := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(cfg.ProviderBaseURL),
	)

	s3Cli, err := newS3Client(ctx, cfg.S3)
	if err != nil {
		return fmt.Errorf("build s3 client: %w", err)
	}
	fsys := s3fs.New(s3Cli, cfg.Bucket)

	tools, err := webhook.SelectBuiltins(cfg.Tools)
	if err != nil {
		return err
	}

	pool, err := mcpclient.NewPool(ctx, toMCPSpecs(cfg.MCPServers), lg)
	if err != nil {
		return fmt.Errorf("connect mcp servers: %w", err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			lg.Warn("closing mcp pool", "err", err)
		}
	}()
	tools = append(tools, pool.Tools()...)

	runner := webhook.NewAgentRunner(webhook.RunnerDeps{
		AgentName:         cfg.AgentName,
		Model:             cfg.Model,
		SystemPrompt:      cfg.SystemPrompt,
		Client:            openaiCli,
		FS:                fsys,
		Tools:             tools,
		Logger:            lg,
		PerRequestTimeout: cfg.PerRequestTimeout,
		ParallelToolCalls: cfg.EffectiveParallelToolCalls(),
	})

	// ctx is the SIGTERM-driven context used for the HTTP server lifecycle;
	// drainCtx is the long-lived context handed to in-flight agent goroutines
	// so they can keep running after the listener closes. If the drain
	// deadline expires we cancel drainCtx to force remaining agents to stop.
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()

	var wg sync.WaitGroup
	launcher := webhook.NewBackgroundLauncher(drainCtx, runner, &wg)

	handler := webhook.Router(launcher, secret, *maxBodyBytes, lg)
	if *metricsEnable {
		handler = withMetrics(handler)
	}

	srv := &http.Server{
		Addr:              *bind,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() {
		lg.Info("listening",
			"bind", *bind,
			"agent", cfg.AgentName,
			"model", cfg.Model,
			"tools", toolNames(tools),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		lg.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *drainTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		lg.Warn("http shutdown", "err", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		lg.Info("all agents drained")
	case <-shutdownCtx.Done():
		lg.Warn("drain timeout exceeded; cancelling in-flight agents")
		cancelDrain()
		<-done
	}

	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func newS3Client(ctx context.Context, s3cfg webhookconfig.S3Config) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(s3cfg.EffectiveRegion()),
	)
	if err != nil {
		return nil, err
	}
	endpoint := s3cfg.EffectiveEndpoint()
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = s3cfg.EffectivePathStyle()
	}), nil
}

func toMCPSpecs(in []webhookconfig.MCPServer) []mcpclient.ServerSpec {
	out := make([]mcpclient.ServerSpec, 0, len(in))
	for _, s := range in {
		out = append(out, mcpclient.ServerSpec{
			Name:      s.Name,
			Transport: s.Transport,
			URL:       s.URL,
			Command:   s.Command,
			Env:       s.Env,
		})
	}
	return out
}

func toolNames(tools []agentloop.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

func withMetrics(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", next)
	return mux
}
