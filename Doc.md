# llm-pivot documentation

See [README.md](README.md) for an overview and screenshot.

## Contents

- [Why](#why)
- [Design notes](#design-notes)
- [Install](#install)
- [Running under systemd](#running-under-systemd---user)
- [Development](#development)
- [The monitor page](#the-monitor-page)
- [Routing OpenAI requests](#routing-openai-requests)
- [Anthropic interface and Claude CLI](#anthropic-interface)
- [Configuration file](#configuration-file)
- [How engine and model options are merged](#how-engine-and-model-options-are-merged)
- [Command-line flags](#command-line-flags)
- [HTTP API](#http-api)
- [Troubleshooting](#troubleshooting)

## Why

Different models may need different engines, such as llama.cpp, strata, maya, splash, or
ninfer. When GPU memory only fits one model, switching requires stopping one
engine and starting another with its model-specific options.

llm-pivot handles that switch when a request names a model. OpenAI and
Anthropic clients share one router port, and the monitor page shows the
active engine's status and output.

## Design notes

- A lock serializes engine starts. Each start stops the previous process.
- Engines run in separate process groups so stop signals reach their children.
- Before starting an engine, llm-pivot checks that its port is free. Otherwise,
  an unrelated process could answer the health probe.
- The router has no write timeout or upstream response-header timeout.
  Incoming request headers have a 30-second timeout.
- Configuration edits are copied, validated and written to disk before they
  become active.
- The page polls once a second for status and new log lines. Polling leaves
  form inputs alone while you type.

## Install

Requires Go 1.26.6 or newer and a Unix-like system with process-group signals
(the service instructions below use Linux/systemd).

```sh
cd ~/GITHUB/llm-pivot
go build -o llm-pivot .
```

The web page is compiled into the binary. First start writes a default
configuration:

```sh
./llm-pivot
# llm-pivot: created default configuration /home/you/.config/llm-pivot/config.yaml
```

Open <http://127.0.0.1:8061>. The generated configuration contains a sample
llama.cpp engine with no models. Set its command to your engine executable and
add a model before starting it. [config.example.yaml](config.example.yaml)
shows llama.cpp and strata definitions. Executables can be found through
`PATH`; for home-directory paths use `{{home}}` because `~` is not expanded.

Check a configuration without starting anything:

```sh
./llm-pivot -check
```

## Running under systemd (`--user`)

```sh
mkdir -p ~/.config/systemd/user
cp llm-pivot.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now llm-pivot
```

To inspect or manage the service:

```sh
systemctl --user status  llm-pivot
systemctl --user restart llm-pivot
systemctl --user reload  llm-pivot     # re-read the YAML file (SIGHUP)
journalctl --user -u llm-pivot -f
```

Keep it running without a login: `loginctl enable-linger $USER`.

The unit assumes the binary at `~/GITHUB/llm-pivot/llm-pivot` and the config
at `~/.config/llm-pivot/config.yaml`; edit `ExecStart` if yours differ.

On stop, llm-pivot sends `SIGTERM` to the engine's process group. If it does
not exit within the graceful stop timeout, llm-pivot sends `SIGKILL`.

After updating the source, rebuild the binary and restart the service:

```sh
go build -o llm-pivot .
systemctl --user restart llm-pivot
```

Reloading with `systemctl --user reload llm-pivot` only rereads the
configuration; it does not load a rebuilt binary. Restarting also stops the
active engine, which starts again on the next model request.

## Development

```sh
go test ./...          # unit + end-to-end tests (spawn real child processes)
go test -race ./...
go vet ./...
```

With Node.js installed, `go test` also runs the monitor scrolling tests in
`web_scroll_test.mjs`; otherwise that test is skipped. Run it directly with
`node web_scroll_test.mjs`.

The end-to-end tests re-execute the test binary as a stand-in LLM engine,
exercising real spawning, health waiting, killing, model switching, and the
config editing API. Anthropic regression tests cover CLI-style conversations,
streamed tool arguments, system-message placement, thinking controls, token
counting and effort compatibility. These tests use simulated engines.

## The monitor page

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ ⚙ llm-pivot   ● llama.cpp · qwen3-30b — running 4m    router :8078           │
├────────────────────────┬─────────────────────────────────────────────────────┤
│ ⚙ Settings             │  Model · qwen3-30b                                  │
│                        │  ┌───────────────────────────────────────────────┐  │
│ ENGINES              + │  │ STATE ● running  PID 4211  PORT 8080  UP 4m   │  │
│ ▸ llama.cpp :8080  ↑ ↓ │  └───────────────────────────────────────────────┘  │
│   ● qwen3-30b      ↑ ↓ │  Model name       [ qwen3-30b                    ]  │
│   ○ gemma3-27b     ↑ ↓ │  Options          [ -m /models/qwen3-30b.gguf    ]  │
│   + Add model          │  [Save model] [Restart] [Stop] [Copy model] [Delete]│
│ ▸ strata    :8090  ↑ ↓ │  Engine monitor · llama.cpp · qwen3-30b             │
│   ○ phi4-mini      ↑ ↓ │  ┌───────────────────────────────────────────────┐  │
│   + Add model          │  │ 12:05:51 ✓ engine "llama.cpp" is serving ...  │  │
│ ~/.config/…yaml        │  └───────────────────────────────────────────────┘  │
└────────────────────────┴─────────────────────────────────────────────────────┘
```

### Left pane

Engines in bold with their port; models are indented below each. A coloured
light shows a model's run state:

| Light             | Meaning                   |
|-------------------|---------------------------|
| ● green           | running and answering     |
| ● amber (pulsing) | starting                  |
| ● amber           | stopping                  |
| ● red             | the engine failed or died |
| ○ grey            | not running               |

Select **Settings**, an engine or a model to open its form on the right. Use
**+** to add an engine, or **Add model** under an engine to add a model.

### Reordering engines and models

Every engine and model row carries **↑** and **↓**. A click moves that entry
one place among its siblings and writes the new order to the configuration
file immediately; there is no separate save step. The arrows are greyed out
at the first and last position, and a model only moves within its own engine
— to put a model under a different engine, delete it and add it there.

Order is presentational. It sets the order of this pane, of the entries in
the configuration file, and of `GET /v1/models`. Nothing starts, stops or
becomes a default because of its position, and moving an entry never
interrupts a running engine.

### Right pane

The right pane contains the selected settings form and an **Engine monitor**
for the active engine.

On wide screens, the form and monitor scroll independently within a fixed page
layout. The monitor keeps the newest line in view while you are at the end of
the output. Scroll up to read earlier output and the view stays where you put
it; slide the window back down to the end and it follows new lines again. At
widths of 880 pixels or less, the page uses normal document scrolling.

The monitor fills the space below the form. Drag the grey handle to adjust its
height. The height resets when you select another page.

### Adding an engine

Click **+** next to ENGINES:

| Field                    | Meaning                                                                                             |
|--------------------------|-----------------------------------------------------------------------------------------------------|
| **Name**                 | Shown in the left pane; must be unique.                                                             |
| **Port**                 | The port the engine listens on. llm-pivot proxies to `127.0.0.1:<port>`. Must differ from the monitor and router ports. |
| **Command**              | Program and default options, e.g. `/path/llama-server -ctx 64000`. Quoting is shell-like. Commands run without a shell: no globbing, `~`, `$VAR`, or pipes. |
| **Monitoring info from** | Source of the monitor content; see [Monitoring modes](#monitoring-modes).                  |
| **Monitor URL**          | For `http` and `embed` modes.                                                                       |
| **Health path**          | Polled until it answers 2xx/3xx. Empty means "an open port is enough".                              |
| **Port flag**            | Injected with the engine port unless already set (default `--port`). `-` disables.                  |
| **Startup timeout**      | Seconds to wait for the port and health probe; `0` uses the global value. Does not cover loading after readiness. |
| **Working directory**    | Optional `cwd`.                                                                                     |
| **Environment**          | Extra `KEY=VALUE` lines.                                                                            |

### Adding a model

Click **Add model** under an engine:

| Field                         | Meaning                                                                             |
|-------------------------------|-------------------------------------------------------------------------------------|
| **Model name**                | What clients put in `"model"`. Must be unique across all engines.       |
| **Additional engine options** | Extra flags for this model. A repeated flag replaces the engine's value for it. |

The **Effective command line** box shows the command after merging the saved
engine and model options.

### Monitoring modes

| `monitor:`          | The right pane shows                                | Use when                                   |
|---------------------|-----------------------------------------------------|--------------------------------------------|
| `stdio` *(default)* | the engine's stdout/stderr, live                    | the engine reports status in its logs        |
| `http`              | the body of `monitor_url`, re-fetched each interval | the engine exposes metrics as text or JSON |
| `embed`             | `monitor_url` itself, rendered in a frame           | the engine has its own monitoring page     |

`http` and `embed` get a second tab, **Process output**. `embed` also gets a
**↻** button and **Open ↗**.

URL fragments such as `#monitor` work in `embed` mode. In `http` mode,
llm-pivot fetches the URL server-side, where fragments have no effect. An
engine that sends `X-Frame-Options: DENY` cannot be framed; use `http` or
**Open ↗**.

In `embed` mode, llm-pivot proxies `monitor_url` through `/engine/` on the
monitor port. The browser only needs access to that port. This also lets a
remote browser view an engine page hosted on the llm-pivot machine's loopback
interface. The URL may point to any address reachable from that machine.

The frame loads once the engine is `running`. While it starts, the frame shows
a waiting message. Check **Process output** for startup logs.

Everything below `/engine/` is forwarded, so a page built from relative links
works unchanged. A page that loads sub-resources by absolute path (`/app.js`)
does not: those escape the prefix and hit llm-pivot's own page instead.

```yaml
monitor: embed
monitor_url: http://127.0.0.1:{{port}}/#monitor
```

### Starting, stopping, restarting

On a model page:

- **Start** stops the current engine, starts the selected model and waits for
  its readiness probe to succeed.
- **Restart** stops and starts the selected model.
- **Stop** stops the current engine.
- **Copy model** creates `<name>-copy` with the saved options. It leaves the
  current engine running. The copy fails if that name already exists.

Changing the active engine's command, port, monitoring mode/URL, health path,
port flag, working directory or environment stops it. Renaming or deleting the
active engine/model, or changing the active model's options, also stops it;
start it again afterwards. Saving unchanged values or editing another model
leaves it running. Startup timeout changes apply on the next start; HTTP
polling interval changes also take effect on the next start.

An engine also stops by itself once `inactive_stop_timeout_seconds` passes
with no proxied request, freeing the VRAM. Only proxied requests keep it
alive; `/v1/models` and `/router/status` do not. A request in flight always
counts. Set `-1` to disable automatic idle stopping. Inactivity is checked
every 30 seconds, so stopping can occur up to about 30 seconds after the
timeout. An omitted or zero timeout uses the default of 600 seconds.

### Settings

Monitor port, router port, bind address, the three timeouts and the log buffer
size. Saving a changed address or port reopens the affected listeners after
the response; the running engine is untouched. Moving a listener can interrupt
requests still using the old address after its three-second shutdown grace.

The bind address defaults to `127.0.0.1`. Setting it to `0.0.0.0` exposes both
ports to the network. Neither port has authentication.

## Routing OpenAI requests

Point any OpenAI-compatible client at the router port:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:8078/v1
export OPENAI_API_KEY=unused
```

```sh
curl -s http://127.0.0.1:8078/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-30b","messages":[{"role":"user","content":"hello"}]}'
```

The router reads the `"model"` field and looks up the configured model. If
that model is already running, it forwards the request. Otherwise it stops the
current engine, starts the requested model and waits for readiness before
forwarding. An unknown model returns `404` with an OpenAI-shaped error and
leaves the current engine running.

The request path and body are forwarded without rewriting the configured model
name, and streaming responses are flushed immediately. The engine must accept
that model name itself if it validates the `"model"` field.

Requests without a model go to the current engine. GET/HEAD bodies and bodies
with a non-JSON Content-Type (such as multipart uploads) are not inspected.
The `?model=<name>` query parameter takes precedence over a JSON model field.

A request for a different model stops the current engine even if it has
requests in flight, so concurrent clients requesting different models can
interrupt one another.

To find the `"model"` field, the router buffers up to 64 MiB of the body.
Larger bodies are forwarded in full to the current engine without model
inspection. For large requests, such as inline audio or images, use
`?model=<name>` to select a model without inspecting the body.

Two endpoints are answered by llm-pivot itself:

| Endpoint             | Answer                                                                   |
|----------------------|--------------------------------------------------------------------------|
| `GET /v1/models`     | Every configured model, each with `owned_by` (the engine) and `running`. |
| `GET /router/status` | The runtime status the monitor page shows.                               |

### Response headers (`X-Llm-Pivot*`)

Router replies include these headers:

| Header                 | On                        | Meaning                                         |
|------------------------|---------------------------|-------------------------------------------------|
| `X-Llm-Pivot`          | every reply               | the version                                     |
| `X-Llm-Pivot-Upstream` | proxied replies only  | `<engine>/<model>:<port>` identifying the engine |

`X-Llm-Pivot-Upstream` identifies the engine that answered. In the Anthropic
interface, llm-pivot translates that response, and errors found while
translating it also carry the header. Errors before an engine response,
such as an unknown model or a connection failure, omit it.

```sh
$ curl -sD- -o/dev/null localhost:8078/chat/completions -d '{"model":"my-model"}'
HTTP/1.1 404 Not Found
X-Llm-Pivot: 1.0.0
X-Llm-Pivot-Upstream: strata/my-model:8024     ← the engine said no
```

The monitor logs HTTP error statuses returned by the engine. Repeats of the
same status/method/path are suppressed for 10 seconds.

llm-pivot's own error payloads follow the OpenAI shape:

| Code                  | HTTP| When                                     |
|-----------------------|-----|------------------------------------------|
| `invalid_request_error` | 400 | the request body could not be read       |
| `model_not_found`     | 404 | the model is not configured              |
| `engine_start_failed` | 503 | the engine would not start               |
| `no_engine_running`   | 503 | nothing runs and no model named          |
| `engine_starting`     | 503 | the engine is still coming up            |
| `engine_unavailable`  | 502 | the engine stopped answering mid-request |

## Anthropic interface

The router recognizes each API by its endpoint. OpenAI requests such as
`POST /v1/chat/completions` are proxied to the engine; `POST /v1/messages`
is translated to OpenAI Chat Completions. Both clients can use the same port
at the same time. No interface switch is needed. The old `router_interface`
setting is ignored and removed on the next configuration save.

### Client setup

| Client | Base URL | Model |
|--------|----------|-------|
| OpenAI-compatible | `http://127.0.0.1:8078/v1` | configured model name |
| Anthropic-compatible | `http://127.0.0.1:8078` | configured model name |

Anthropic clients append `/v1/messages` themselves. Use a dummy API key if
required by the client; llm-pivot does not authenticate requests. Engine
commands and model names stay the same.

```sh
curl http://127.0.0.1:8078/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"qwen3-30b","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}'
```

`model`, a positive `max_tokens`, and a nonempty `messages` array are
required. `?model=<name>` overrides the model in a valid request body.

`POST /v1/messages/count_tokens` takes the same body without `max_tokens`
and answers `{"input_tokens": N}`. See
[Token counting](#token-counting) for how the count is produced.

### Claude CLI

With Claude Code installed, point the CLI at the router and select a model
configured in llm-pivot:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8078 \
ANTHROPIC_AUTH_TOKEN=unused \
claude --model qwen3-30b
```

Replace `qwen3-30b` with your configured model name. Requests naming an
unconfigured model return `404`, so any other models the client requests
must also be configured. See Claude Code's [gateway setup](https://code.claude.com/docs/en/llm-gateway-connect)
and [model configuration](https://code.claude.com/docs/en/model-config).

### Request translation

The adapter supports text, images (URL or base64), custom tools, text tool
results, tool choice and sampling controls. It returns Anthropic message
objects or SSE events, including incremental tool arguments. The engine
must support the corresponding OpenAI features.

| Anthropic option | Engine request |
|------------------|----------------|
| `system` | text in the initial system message |
| `max_tokens` | `max_tokens` |
| `temperature`, `top_p`, `top_k` | same keys |
| `stop_sequences` | `stop` |
| custom `tools` | function tools with the supplied input schemas |
| `tool_use` / `tool_result` blocks | assistant tool calls / tool messages |
| `tool_result` with `is_error` | tool message text prefixed with `Error:` |
| `output_config.format` (`json_schema`) | `response_format` with the supplied schema |
| `output_config.effort` | `reasoning_effort`; `max` becomes `xhigh` |

Unknown top-level options and unsupported content blocks return `400` with
an explanation. `metadata` is accepted without being sent to the engine.
Prompt-cache annotations on content blocks have no effect.

### System messages and effort changes

Claude CLI can add `system` messages inside the conversation. The adapter
collects their active text, in order, after the top-level system text and
sends one system message at the beginning. This supports templates that
raise `System message must be at the beginning`. Later instructions move
ahead of the history, so their original timing is not preserved. User and
assistant turns and tool calls/results keep their order.

A system reminder with `clear_at: next_user_message` is omitted once a later
user message exists. Reminders without `clear_at`, or with `clear_at: never`,
remain active. Empty messages that only change effort add no prompt text.

Per-message `output_config.effort` overrides the top-level effort for the
current generation; the last change wins. Per-message output configuration
supports effort only. Disabled thinking overrides all effort settings.

### Thinking and engine effort levels

| `thinking.type` | Engine controls |
|-----------------|-----------------|
| `enabled` | `chat_template_kwargs.enable_thinking: true` and `reasoning_budget_tokens` from `budget_tokens` |
| `adaptive` | `chat_template_kwargs.enable_thinking: true`; uses the engine's configured budget and requested effort |
| `disabled` | `chat_template_kwargs.enable_thinking: false` and `reasoning_effort: none` |

For enabled thinking, `budget_tokens` must be at least 1024 and less than
`max_tokens`. Thinking controls are llama.cpp extensions; other engines may
ignore them.

Reasoning an engine reports as `reasoning_content` is returned as a
`thinking` block ahead of the answer, streamed as `thinking_delta`. A local
engine cannot produce a Claude signature, so the block carries an empty one
and clients that replay it get their reasoning back as `reasoning_content`.
Redacted thinking in history is dropped, since nothing local can read it.
`thinking.display` is validated but does not hide the block: engine
reasoning is plain text that the engine already exposes in full.

Effort values `low`, `medium`, `high` and `xhigh` are forwarded unchanged;
`max` maps to `xhigh`. Templates differ in the values they accept. If an
engine returns `400` or `500` with `Unexpected reasoning effort ...` and
`Supported types are ...`, the adapter retries once with the nearest listed
level, preferring the higher level on a tie. For example, a template accepting
`xhigh`, `medium` and `low` gets `xhigh` when it rejects `high`.

The monitor logs the replacement. Other errors and responses that have
started streaming are not retried. Disabled thinking is never changed to an
enabled effort level.

### Response details

`GET /v1/models` returns Anthropic format when the request includes an
`anthropic-version` header; otherwise it returns OpenAI format. It lists all
configured models without pagination. `/router/status` and the monitor API
remain available. Errors on `/v1/messages` and its subpaths use Anthropic
error objects; errors on OpenAI endpoints use OpenAI error objects.

Token counts come from the engine; unavailable counts are zero. Stop reasons
are mapped to `end_turn`, `max_tokens` or `tool_use`. The adapter cannot recover
which stop sequence matched from an OpenAI response, so `stop_sequence` is null.

### Token counting

`POST /v1/messages/count_tokens` validates and translates the request the
same way `/v1/messages` does, then counts the prompt the engine would
actually receive. Engines expose no common token counter, so the adapter
tries llama.cpp's native endpoints first: `/apply-template` renders the
prompt and `/tokenize` counts it, neither of which generates anything. An
engine missing either endpoint gets an estimate of four characters per
token, and the monitor logs that counts for the model are estimated.
`X-Llm-Pivot-Upstream` is present only on exact counts.

Counting starts the engine for the named model, as any other request does.

### Limits

Requests are limited to 64 MiB. Document blocks, server tools and non-text
tool results are rejected. Batches and the remaining Anthropic
endpoints are not implemented. Compatibility depends on the engine and
client features in use; this adapter does not connect to Anthropic's hosted
models. See the [Messages reference](https://platform.claude.com/docs/en/api/http/messages)
and [streaming protocol](https://platform.claude.com/docs/en/build-with-claude/streaming).

## Configuration file

The configuration path is selected in this order:

1. `-config <path>`
2. `$LLM_PIVOT_CONFIG`
3. `$XDG_CONFIG_HOME/llm-pivot/config.yaml`
4. `~/.config/llm-pivot/config.yaml`

The page writes this file on every change. Hand-written comments are not
preserved once you save from the page.

The order of `engines:` and of each `models:` list is the order the left pane
shows, and a rewrite keeps it. Reorder from the page rather than by hand: the
next save from the page would overwrite an edit made while it was open.

```yaml
monitor_port: 8061
router_port: 8078
host: 127.0.0.1
startup_timeout_seconds: 180          # how long an engine may take to answer
graceful_stop_timeout_seconds: 20     # SIGTERM grace before SIGKILL
inactive_stop_timeout_seconds: 600    # stop an idle engine after this; -1 never
log_lines: 4000

engines:
  - name: llama.cpp
    port: 8080
    command: >-
      /home/vmakarov/GITHUB/llama.cpp/build/bin/llama-server
      --n-gpu-layers 99
      --ctx-size 64000
      --flash-attn on
    monitor: stdio            # or: http, embed
    health_path: /health
    models:
      - name: qwen3-30b
        options: -m /models/Qwen3-30B-A3B-Q4_K_M.gguf
```

### Keys

#### Top level

| Key                             | Default     | Meaning                                    |
|---------------------------------|-------------|--------------------------------------------|
| `monitor_port`                  | `8061`      | web page port                              |
| `router_port`                   | `8078`      | OpenAI and Anthropic API port                                |
| `host`                          | `127.0.0.1` | bind address for both ports                |
| `startup_timeout_seconds`       | `180`       | wait for TCP and optional health probe                      |
| `graceful_stop_timeout_seconds` | `20`        | `SIGTERM` grace before `SIGKILL`           |
| `inactive_stop_timeout_seconds` | `600`       | stop an idle engine after this; `-1` never |
| `log_lines`                     | `4000`      | monitoring buffer size; resized live       |

#### Per engine

| Key                        | Default  | Meaning                                                                           |
|----------------------------|----------|-----------------------------------------------------------------------------------|
| `name`                     | required | unique engine name                                                                |
| `port`                     | required | where the engine listens                                                          |
| `command`                  | required | program and default options                                                       |
| `monitor`                  | `stdio`  | `stdio`, `http` or `embed`; aliases `logs`→`stdio`, `iframe`/`page`/`web`→`embed` |
| `monitor_url`              | —        | required for `http` or `embed`                                                    |
| `monitor_interval_seconds` | `5`      | polling interval (`http` only)                                                    |
| `health_path`              | —        | startup probe path; empty = TCP connect only                                      |
| `port_flag`                | `--port` | injected unless present; `-` disables                                             |
| `startup_timeout_seconds`  | `0`      | positive values override the global value                                                        |
| `work_dir`                 | —        | working directory                                                                 |
| `env`                      | —        | list of `KEY=VALUE`                                                               |
| `models`                   | —        | list of models                                                                    |

#### Per model

| Key       | Meaning                                                       |
|-----------|---------------------------------------------------------------|
| `name`    | unique across all engines; what clients send as `"model"` |
| `options` | extra flags; override the engine's own flags                  |

### Placeholders

Usable in `command`, `options`, `monitor_url`, `work_dir`, `env`: `{{port}}`,
`{{engine}}`, `{{model}}`/`{{name}}`, `{{host}}`, `{{home}}`.

Use `{{home}}/models/model.gguf` instead of `~/models/model.gguf`; shell
expansion does not occur.

`{{host}}` is always `127.0.0.1`, the address used for engine health checks
and routed requests. Use `--host {{host}}` when the engine needs an explicit
bind address.

### Startup validation

llm-pivot refuses to start on: unparseable YAML or an unknown option, a
duplicated or empty engine/model name, a bad or conflicting listener port, an
empty or unparseable `command`, malformed model options, an unknown monitoring
mode, `http` or `embed` with no `monitor_url`, a nonempty `health_path`
without a leading `/`, or an `env` entry that is not `KEY=VALUE`. Engine ports
may be shared across engines, since only one runs at a time. Exit status is
`1` for validation failures. Validation does not check whether executable or
model files exist; those problems surface when starting or using the engine.

## How engine and model options are merged

The engine's `command` carries the defaults; a model's `options` layer on top:

- a flag present in both keeps its first position in the engine command and
  takes the model's value; other engine occurrences of that flag are dropped;
- a flag only the model has is appended, as are bare operands;
- `--flag=value` and `--flag value` are the same flag;
- `-1` after a flag is a value, not a flag (`--seed -1` survives);
- finally `--port <port>` is appended unless already set.

```
engine command : /bin/llama-server --ctx-size 64000 --device Vulkan1
model options  :                   --ctx-size 8192  -m /models/x.gguf
─────────────────────────────────────────────────────────────────
executed       : /bin/llama-server --ctx-size 8192 --device Vulkan1 \
                            -m /models/x.gguf --port 8080
```

You cannot remove an engine flag from a model; give it the value you want
(`--flash-attn off`). The merged line is shown on each model page and written
to the monitor as `$ …` when an engine starts.

## Command-line flags

```
-config string         path of the YAML configuration file
-monitor-port int      override monitor_port
-router-port int       override router_port
-host string           override the bind address
-init                  write a default configuration file and exit
-check                 validate the configuration file and exit
-version               print the version and exit
```

Command-line overrides apply for the current run. Saving from the page keeps
the file's own `monitor_port`, `router_port` and `host` values unless you
change them in Settings. A reload reapplies the overrides after reading the
file.

Settings shows the effective values, including any overrides. Entering a
different value saves it to the file and drops that override for the current
run. The form reports which override was dropped. Starting the program again
with the same flags reapplies them.

`-check` validates the file on its own and then once more with the overrides
applied, so a flag that collides with an engine port is reported too.

## HTTP API

The page uses these endpoints on the monitor port. Scripts can call them
directly.

| Method & path                                 | Body / result                                              |
|-----------------------------------------------|------------------------------------------------------------|
| `GET /api/state?since=<seq>`                  | configuration, status, log lines newer than `seq`          |
| `GET /api/state?…&monitor_body=1`             | the same, plus the sampled body of an `http` monitor       |
| `GET /api/command?model=<name>`               | the effective command line                                 |
| `PUT /api/settings`                           | `{monitor_port, router_port, host, …timeouts…, log_lines}` |
| `POST /api/engines`                           | engine fields plus `original` (empty = create)             |
| `DELETE /api/engines/{engine}`                | removes the engine and its models                          |
| `POST /api/engines/{engine}/move`             | `{"direction":"up"\|"down"}`                               |
| `POST /api/models`                            | `{engine, original, name, options}`                        |
| `DELETE /api/engines/{engine}/models/{model}` |                                                            |
| `POST /api/engines/{engine}/models/{model}/move` | `{"direction":"up"\|"down"}`                            |
| `POST /api/control`                           | `{"action":"start"\|"stop"\|"restart","model":"<name>"}`   |
| `POST /api/log/clear`                         | empties the monitoring buffer                              |
| `GET /engine/…`                               | the running `embed` engine's own page, proxied             |

API rejections come back as `{"error":"…"}` with a 4xx status. JSON request
bodies are limited to 1 MiB. Engine/model saves replace their editable fields,
so send the complete definition; `original` identifies the existing name when
updating or renaming. Configuration edits return `notes`, `ports_moved`,
`monitor_port` and `router_port`.

`/api/state` also returns `log_seq` and `log_gap`. Use `log_seq` as the next
`since` value; `log_gap` means older requested lines were cleared or dropped
from the buffer.

The monitor supports links to individual forms: `#settings`, `#engine/<name>`,
`#model/<engine>/<model>`.

## Troubleshooting

### Service will not start

Run the binary by hand; the reason is on stderr and in
`journalctl --user -u llm-pivot`.

### Engine will not start

Its output is in the monitor pane, under the `$ …` line. Usual causes: wrong
path, unknown option, missing model file.

### "did not answer on port N"

The engine started but never served `health_path`. It may need more time,
listen on a different port, or bind an address other than loopback.

### "exited during startup"

The process died; exit status and last output are in the pane above the
message.

### "port N is already in use"

Another process is listening on the engine port. Find it with
`ss -ltnp 'sport = :N'`.

### A browser window opens on start

Check the engine command for a flag that opens a browser, such as strata's
`--open`, and remove it. To view the engine page inside llm-pivot, use
`monitor: embed`.

### Monitor shows raw HTML

You are in `http` mode. Switch the engine to `embed`.

### Slow model startup

The startup timeout covers readiness checks. Loading triggered by the first
generation request has no router deadline:

| Wait                                       | Covered by                | Typical        |
|--------------------------------------------|---------------------------|----------------|
| process start → port open and health probe succeeds | `startup_timeout_seconds` | seconds to minutes |
| first request → first token (weights load) | nothing                   | can be minutes |

Many engines open the port immediately and load lazily on the first request.
llm-pivot sets no deadline on the second wait. A client timeout cancels the
request, including during model loading. Raise the client timeout, or send a
warm-up request through the router. Starting a model from the monitor page
only waits for the configured readiness probe; it does not send a generation
request.

### The port will not change

If the new port is taken, the rebind fails and the old port keeps working. The
new setting is still saved, so correct it in the configuration before
restarting the service.

### Model names and the router

The router dispatches purely on the configured model name, which must be
unique across engines. The name need not match the model filename, but the
engine must accept it in forwarded requests (configure an engine-side alias if
needed).

### Nothing proxied / 503

Either no engine is running and no model was named, or the engine is still
loading. `GET /router/status` says which.

### 404 from a client, but the engine runs

Check `X-Llm-Pivot-Upstream`. If it is present, the engine returned the 404.
Check whether the client base URL is missing `/v1`:

```
http://127.0.0.1:8078/v1   ✅
http://127.0.0.1:8078      ❌  →  POST /chat/completions  →  404
```

Ask the engine directly to see what it serves:

```sh
curl -s localhost:<engine port>/v1/models
```

### Claude CLI compatibility errors

`unsupported Anthropic option "output_config"`, `extended thinking is not
supported by this adapter`, and `message role must be user or assistant`
were errors from older adapter versions. Current code supports output
configuration, enabled/adaptive/disabled thinking, and system messages.
If those exact errors persist after updating, rebuild and restart llm-pivot;
a configuration reload does not replace the running binary.

For `Unexpected reasoning effort high`, current code retries once using the
engine's reported supported values. Check the monitor for the replacement.
`System message must be at the beginning` is handled by combining active
system instructions into the initial message. These translations apply to
Anthropic `/v1/messages` requests; OpenAI request bodies are passed through.

For a different `400`, read the option or block named in the error. For a
remaining engine `500`, inspect its process output for the underlying error,
such as memory exhaustion or an inference failure.
