# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`mithras` — Native AI agent objects for Kubernetes. Go module `github.com/tigrisdata-community/mithras` (Go 1.26.2). Currently a library of building blocks; no `cmd/` binaries yet.

## Commands

Build, vet, and unit tests (all packages):

```
go build ./...
go vet ./...
go test ./...
```

Run a single test:

```
go test ./internal/agentloop -run TestName
go test ./internal/codeinterpreter/python -run TestRunWritesToRoot -v
```

`internal/s3fs` tests are **integration tests that hit a real S3-compatible endpoint**. They require `-endpoint` and `-bucket` flags and credentials in the environment. Example against Tigris:

```
AWS_PROFILE=tigris-dev go test -count=1 -timeout=5m ./internal/s3fs/... \
  -endpoint=https://fly.storage.tigris.dev -bucket=xe-mithras-s3fs-test
```

The test bucket must already exist and be writable. Tests create and clean their own keys but reuse the bucket.

## Architecture

Four packages under `internal/`, composed bottom-up:

1. **`internal/s3fs`** — Fork of `jszwec/s3fs` (vendored as an internal package, original license preserved) extended with write support. `S3FS` implements `fs.FS`, `fs.StatFS`, `fs.ReadDirFS` plus the package-local `CreateFS`, `WriteFileFS`, `RemoveFS`, `MkdirAllFS` interfaces. S3 is flat; directories are simulated with `/`-delimited prefixes and zero-byte marker objects written by `MkdirAll`. `WazeroFS` (in `wazerofs.go`) adapts `*S3FS` to `wazero/experimental/sys.FS` via `(*S3FS).AsWazeroFS()` so the bucket can be mounted as a **writable** filesystem inside a WASI guest.

2. **`internal/codeinterpreter/python`** — Embeds `python.wasm` (~25 MB, committed to the repo) and runs it under `tetratelabs/wazero` with WASI preview 1. The wazero runtime and compiled module are package-level globals initialized in `init()`. `Run(ctx, fsys, userCode)`:
   - Writes `main.py` to a host temp dir and mounts that dir at `/.mithras` in the guest (keeping the script outside any paths the caller's fs might use).
   - Mounts `fsys` at `/`. If `fsys` implements `wazeroMountable` (i.e. has `AsWazeroFS() experimentalsys.FS`), it's mounted via `WithSysFSMount` so the guest can **write back** to the host. Otherwise it falls back to the read-only `WithFSMount` adapter.
   - Captures stdout/stderr into buffers and returns them in `Result`. `PlatformError` is populated only when wazero itself errors (distinct from Python tracebacks, which land in `Stderr`).
   The `emptyFS` type in this package is used when the caller passes `nil` — it answers `"."` but returns ENOENT for everything else, so the guest sees an empty root.

3. **`internal/agentloop`** — Tool-calling agent loop over the OpenAI Go SDK (`github.com/openai/openai-go/v3`; any OpenAI-compatible endpoint works). `Impl` owns the conversation and a `map[string]Tool`; `Run(ctx, prompt, opts...)` appends the prompt, sends a completion request, dispatches any tool calls, appends tool results, and repeats until the model returns `finish_reason == "stop"` or a tool returns a sentinel error.
   - **Sentinels**: tools return `ErrSentinelOkay` to end the loop successfully or `ErrSentinelAbort` to end it unsuccessfully. Non-sentinel tool errors are reported back to the model as a tool message so it can retry.
   - **Retry**: transient completion failures retry up to 5 times with linear backoff.
   - **Concurrency**: `Run` is serialized by an internal mutex; safe to call concurrently.
   - **Metrics**: prompt/completion/cached/reasoning tokens are exported as `mithras_agentloop_tokens_used` (labels `model`, `kind`) and also accumulated into `Result`.
   - **Options**: `EnableParallelToolCalling` sets `ParallelToolCalls = true` on the request params.
   - The `fs.FS` stored on `Impl` is passed through to `Tool.Run` — this is how the Python tool receives the user-visible filesystem.

4. **`internal/tools/python`** — An `agentloop.Tool` implementation that wraps `codeinterpreter/python`. Input is `{"code": string}`; output is the JSON-marshaled `codeinterpreter/python.Result` (stdout/stderr/platformError). The `fs.FS` argument that `agentloop` passes in is what the Python code sees mounted at `/`.

### Composition

The intended stack: an `agentloop.Impl` is constructed with `FS: s3fs.New(...)` and `Tools: []agentloop.Tool{python.Impl{}}`. When the model calls the python tool, user code runs in a WASI sandbox with the S3 bucket mounted at `/` and can both read and write objects directly.

## Conventions

Use the following skills (non-negotiable — invoke them, do not paraphrase from memory):

- **Writing Go code** → use the `go-style` skill. Covers CLI patterns (`flagenv.Parse()` then `flag.Parse()`), error handling (sentinels + `%w` wrapping), `log/slog` usage, HTTP middleware.
- **Writing Go tests** → use the `go-table-driven-tests` skill. Enforces the table-driven shape (`name` field, `tt` loop variable, `t.Parallel()`, `t.Run(tt.name, ...)`).
- **Creating git commits** → use the `conventional-commits` skill.

Source copies of `go-style` and `go-table-driven-tests` are vendored under `.agents/skills/` for reference.