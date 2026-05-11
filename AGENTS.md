# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`mithras` — Native AI agent objects for Kubernetes. Go module `github.com/tigrisdata-community/mithras` (Go 1.26.2). The `cmd/webhookd` binary is the production HTTP front-end; the rest of the codebase is the supporting library and the Kubernetes CRD (`internal/k8s/agent/v1alpha1`) that schedules it.

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

Use `go doc` to find documentation for Go packages. For example: `go doc github.com/mitchellh/go-libghostty`. Full syntax is `go doc [<pkg>.][<sym>.]<methodOrField>` for full help output.

`internal/s3fs` tests are **integration tests that hit a real S3-compatible endpoint**. They require `-endpoint` and `-bucket` flags and credentials in the environment. Example against Tigris:

```
AWS_PROFILE=tigris-dev go test -count=1 -timeout=5m ./internal/s3fs/... \
  -endpoint=https://fly.storage.tigris.dev -bucket=xe-mithras-s3fs-test
```

The test bucket must already exist and be writable. Tests create and clean their own keys but reuse the bucket.

## Architecture

The library composes bottom-up: a writable S3-backed `fs.FS`
(`internal/s3fs`), a Python WASI sandbox that mounts that fs at `/`
(`internal/codeinterpreter/python`), an OpenAI tool-calling loop that
hands the fs to its tools (`internal/agentloop`), and an HTTP webhook
front-end that ties it all together (`cmd/webhookd` and
`internal/webhook`). Kubernetes integration lives in
`internal/k8s/agent/v1alpha1` as a yoke airway/flight pair.

Read [docs/architecture.md](docs/architecture.md) for the per-package
breakdown — sentinel errors, retry/metrics behaviour, the
WASI-mount/wazero details, the webhookd lifecycle, and how the CRD is
realized into Deployment/Service/Ingress objects.

## Conventions

Use the following skills (non-negotiable — invoke them, do not paraphrase from memory):

- **Writing Go code** → use the `go-style` skill. Covers CLI patterns (`flagenv.Parse()` then `flag.Parse()`), error handling (sentinels + `%w` wrapping), `log/slog` usage, HTTP middleware.
- **Writing Go tests** → use the `go-table-driven-tests` skill. Enforces the table-driven shape (`name` field, `tt` loop variable, `t.Parallel()`, `t.Run(tt.name, ...)`).
- **Creating git commits** → use the `conventional-commits` skill.

Source copies of `go-style` and `go-table-driven-tests` are vendored under `.agents/skills/` for reference.
