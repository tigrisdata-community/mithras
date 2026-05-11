# Architecture

`mithras` is a library plus a single binary (`webhookd`) that runs an
OpenAI-compatible tool-calling agent inside a Kubernetes pod. The packages
compose bottom-up from a writable S3 filesystem, through a WASI Python
sandbox, through the agent loop, and out to an HTTP webhook front-end. A
parallel set of packages produces the Kubernetes CRD and operator artifacts
(via [yoke](https://github.com/yokecd/yoke)) that schedule the binary.

## Runtime stack (top-down)

The intended deployment looks like this:

```
HTTP POST /v1/invoke              ← cmd/webhookd
        │
        ▼
internal/webhook (router, auth, background launcher, runner)
        │
        ▼
internal/agentloop.Impl  ──── tools ────►  internal/tools/python
        │                                  internal/mcp adapters
        │
        ▼
internal/s3fs.S3FS        (mounted at "/" inside the WASI guest)
```

A POST to `/v1/invoke` is authenticated, the body is captured, and a
goroutine is spawned that builds a fresh `agentloop.Impl` and drives it to
completion against the configured model and tool set. The S3 bucket is
mounted writable inside the Python sandbox so user code can read and write
objects directly.

## Packages

### `cmd/webhookd`

HTTP front-end. Loads its config from a ConfigMap-mounted YAML file, reads
secrets from environment variables, builds the S3 client, connects MCP
servers, registers built-in tools, and starts an `http.Server`. SIGTERM
triggers a bounded drain: the listener closes, in-flight agent goroutines
keep running on a long-lived context, and if the drain deadline expires
that context is cancelled to force them down.

Flags (also accepted as env vars via `flagenv`): `--config-path`, `--bind`,
`--slog-level`, `--drain-timeout`, `--max-body-bytes`, `--metrics`.

Required env: `OPENAI_API_KEY`, `WEBHOOK_SHARED_SECRET`.

### `internal/agentloop`

Tool-calling agent loop over the OpenAI Go SDK
(`github.com/openai/openai-go/v3`; any OpenAI-compatible endpoint works).
`Impl` owns the conversation and a `map[string]Tool`; `Run(ctx, prompt,
opts...)` appends the prompt, sends a completion request, dispatches any
tool calls, appends tool results, and repeats until the model returns
`finish_reason == "stop"` or a tool returns a sentinel error.

- **Sentinels**: tools return `ErrSentinelOkay` to end the loop successfully
  or `ErrSentinelAbort` to end it unsuccessfully. Non-sentinel tool errors
  are reported back to the model as a tool message so it can retry.
- **Retry**: transient completion failures retry up to 5 times with linear
  backoff.
- **Concurrency**: `Run` is serialized by an internal mutex; safe to call
  concurrently.
- **Metrics**: prompt/completion/cached/reasoning tokens are exported as
  `mithras_agentloop_tokens_used` (labels `model`, `kind`) and also
  accumulated into `Result`.
- **Options**: `EnableParallelToolCalling` sets `ParallelToolCalls = true`
  on the request params.
- The `billy.Filesystem` stored on `Impl` is passed through to
  `Tool.Run` — this is how the Python tool receives the user-visible
  filesystem. Tools may both read and write through it.

### `internal/codeinterpreter/python`

Embeds `python.wasm` (~25 MB, committed to the repo) and runs it under
`tetratelabs/wazero` with WASI preview 1. The wazero runtime and compiled
module are package-level globals initialized in `init()`. `Run(ctx, fsys,
userCode)`:

- Writes `main.py` to a host temp dir and mounts that dir at `/.mithras`
  in the guest (keeping the script outside any paths the caller's fs
  might use).
- Mounts `fsys` (a `billy.Filesystem`) at `/` via the
  `tangled.org/xeiaso.net/kefka/wasm/billyfs` adapter, which exposes
  billy as a wazero `experimental/sys.FS`. This is always a writable
  mount, so the guest can **write back** to the host.
- Captures stdout/stderr into buffers and returns them in `Result`.
  `PlatformError` is populated only when wazero itself errors (distinct
  from Python tracebacks, which land in `Stderr`).

If the caller passes `nil`, `Run` substitutes `memfs.New()` so the guest
sees an empty (but still writable) root.

The mounted filesystem is wrapped in a `dirAwareFS` shim before being
handed to the kefka adapter. memfs and several other billy backends
reject `OpenFile(".", O_RDONLY)`, but wazero's WASI preopen plumbing
needs that call to succeed in order to bind the mount handle. The shim
intercepts directory opens and returns a placeholder `billy.File` whose
I/O methods error — the kefka adapter only reads `Name()` off directory
handles, so this is enough.

### `internal/mcp`

Adapts the Model Context Protocol Go SDK
(`github.com/modelcontextprotocol/go-sdk`) to the `agentloop.Tool`
interface so MCP-provided tools register alongside built-ins. Supports
three transports: `stdio`, `streamable-http`, and `sse`. `Pool` owns a
collection of live clients; tool names are namespaced with the server name
to prevent collisions.

### `internal/s3fs`

Fork of `jszwec/s3fs` (vendored as an internal package, original license
preserved) extended with write support. `S3FS` implements `fs.FS`,
`fs.StatFS`, `fs.ReadDirFS` plus the package-local `CreateFS`,
`WriteFileFS`, `RemoveFS`, `MkdirAllFS` interfaces. S3 is flat;
directories are simulated with `/`-delimited prefixes and zero-byte
marker objects written by `MkdirAll`.

`(*S3FS).AsBilly()` (in `billy.go`) returns a `billy.Filesystem` view of
the same bucket. The agent loop and Python sandbox consume this view —
writes through the billy interface land directly in S3. S3 has no
symlinks, no chmod, and no rename primitive, so `Chroot` is a no-op and
the symlink/`Rename`/`TempFile`/`Truncate` paths return
`billy.ErrNotSupported`.

Tests are **integration tests that hit a real S3-compatible endpoint** —
see `AGENTS.md` for the invocation.

### `internal/tools/python`

An `agentloop.Tool` implementation that wraps `codeinterpreter/python`.
Input is `{"code": string}`; output is the JSON-marshaled
`codeinterpreter/python.Result` (stdout/stderr/platformError). The
`billy.Filesystem` argument that `agentloop` passes in is what the Python
code sees mounted at `/`.

### `internal/webhook`

HTTP-facing pieces of `webhookd`:

- `Router` wires `POST /v1/invoke` (auth-required) and `GET /healthz`,
  with `recover500` and `requestLog` middleware on top.
- `requireToken` extracts the token from an `Authorization: Bearer …`
  header and constant-time compares it against the shared secret. Failed
  requests get a `WWW-Authenticate: Bearer realm="mithras"` response.
- `AgentRunner` builds a fresh `agentloop.Impl` per request so conversation
  histories do not bleed between webhooks.
- `BackgroundLauncher` spawns each runner call on a `sync.WaitGroup` so
  the HTTP shutdown path can drain in-flight agents.
- `BuiltinTools` / `SelectBuiltins` is the registry that maps config-named
  tool strings (currently `"python"`) to `agentloop.Tool` implementations.

### `internal/webhook/webhookconfig`

Parses and validates the on-disk YAML loaded from the ConfigMap. Rejects
unknown fields so typos surface as errors. Performs `${VAR}` env expansion
on MCP server env values. Defaults `S3.Endpoint` to `https://t3.storage.dev`
and `S3.Region` to `auto`, matching Tigris.

### `internal/k8s/agent/v1alpha1`

Go types for the `mithras.tigris.sh/v1alpha1 Agent` custom resource. The
spec carries the model, system prompt, bucket, tool list, ingress
parameters, and a reference to the credentials Secret. `Valid()` enforces
required fields and that `perRequestTimeout` parses as a `time.Duration`.

#### `airway/`

Yoke "airway" entrypoint. Emits a yoke `Airway` document whose CRD schema
is derived (via reflection through `yoke/pkg/openapi`) from the `Agent`
Go type, and points at the flight Wasm module published at
`oci://ghcr.io/tigrisdata-community/mithras/crd/agent/flight:v1alpha1`.
Compiled to `agent-airway.wasm` for installation.

#### `flight/`

Yoke "flight" entrypoint. Reads an `Agent` from stdin and emits the
ConfigMap, webhook-secret Secret, Deployment, Service, and Ingress that
realize it. Looks up the user-supplied credentials Secret (and an existing
`<name>-webhook-secret`, generating a new UUIDv7 if absent) via
`yoke/pkg/flight/wasi/k8s`. Compiled to `agent-flight.wasm` and pushed to
the OCI registry referenced by the airway.

The `build.sh`, `push.sh`, and `deploy.sh` scripts in this directory build
the two Wasm modules, publish the flight image, and apply the airway to a
cluster. `sample.yaml` and `provider-secret.yaml` are example resources.

## Composition

The intended stack: an `agentloop.Impl` is constructed with `FS:
s3fs.New(...).AsBilly()` and `Tools` containing `pythontool.Impl{}` plus
any MCP adapters from a connected `mcp.Pool`. When the model calls the
python tool, user code runs in a WASI sandbox with the S3 bucket mounted
at `/` and can both read and write objects directly. `cmd/webhookd` is
the production assembly of this stack; the operator side
(`internal/k8s/agent/v1alpha1`) packages it as a Kubernetes CRD.
