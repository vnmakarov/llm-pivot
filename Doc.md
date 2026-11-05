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
- [Configuration file](#configuration-file)
- [How engine and model options are merged](#how-engine-and-model-options-are-merged)
- [Command-line flags](#command-line-flags)
- [HTTP API](#http-api)
- [Troubleshooting](#troubleshooting)

---

## Why

My models do not all run on the same engine. Some want llama.cpp, some want
strata or ninfer. Only one can hold the VRAM at a time, so every switch meant
stopping one engine and starting another by hand, with the right flags. Each
engine also reports status wherever it likes.

llm-pivot solves both. One OpenAI port starts whichever engine serves the
requested model, freeing the VRAM first. One page shows the output of whatever
is running.

---

## Design notes

- **One engine, one model, ever.** Every start path funnels through one lock
  that stops the previous process first.
- **Process groups.** Engines run in their own group and die with it.
- **No starting on an occupied port.** A stranded engine would otherwise answer
  the health probe and serve the wrong model.
- **No deadline on generation.** The router sets no write timeout or upstream
  response-header timeout. Incoming request headers have a 30-second timeout.
- **Config is never mutated in place.** Each change is copied, validated,
  written, then published.
- **The page polls once a second** on one endpoint carrying status and new log
  lines; forms are not re-rendered while you type.

---

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
shows llama.cpp and strata definitions. Executables can be found through `PATH`;
for home-directory paths use `{{home}}` because `~` is not expanded.

Check a configuration without starting anything:

```sh
./llm-pivot -check
```

---

## Running under systemd (`--user`)

```sh
mkdir -p ~/.config/systemd/user
cp llm-pivot.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now llm-pivot
```

Everyday commands:

```sh
systemctl --user status  llm-pivot
systemctl --user restart llm-pivot
systemctl --user reload  llm-pivot     # re-read the YAML file (SIGHUP)
journalctl --user -u llm-pivot -f
```

Keep it running without a login: `loginctl enable-linger $USER`.

The unit assumes the binary at `~/GITHUB/llm-pivot/llm-pivot` and the config at
`~/.config/llm-pivot/config.yaml`; edit `ExecStart` if yours differ.

On stop, llm-pivot sends `SIGTERM` to the engine's process group, waits, then
`SIGKILL`s it. No engine is left behind.

---

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
config editing API.

---

## The monitor page

```
┌──────────────────────────────────────────────────────────────────────────┐
│ ⚙ llm-pivot   ● llama.cpp · qwen3-30b — running 4m    router :8078       │
├────────────────────┬─────────────────────────────────────────────────────┤
│ ⚙ Settings         │  Model · qwen3-30b                                  │
│                    │  ┌───────────────────────────────────────────────┐  │
│ ENGINES        +   │  │ STATE ● running  PID 4211  PORT 8080  UP 4m   │  │
│ ▸ llama.cpp :8080  │  └───────────────────────────────────────────────┘  │
│     ● qwen3-30b    │  Model name       [ qwen3-30b                    ]  │
│     ○ gemma3-27b   │  Options          [ -m /models/qwen3-30b.gguf    ]  │
│     + Add model    │  [Save model] [Restart] [Stop] [Copy model] [Delete]│
│ ▸ strata    :8090  │  Engine monitor · llama.cpp · qwen3-30b             │
│     ○ phi4-mini    │  ┌───────────────────────────────────────────────┐  │
│     + Add model    │  │ 12:05:51 ✓ engine "llama.cpp" is serving ...  │  │
│ ~/.config/…yaml    │  └───────────────────────────────────────────────┘  │
└────────────────────┴─────────────────────────────────────────────────────┘
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

Click anything to open it on the right: **Settings**, an **engine**, a
**model**, **+ (add engine)**, **Add model**.

### Right pane

Two parts: the thing you clicked (its settings and buttons), and the live
**Engine monitor** for whatever is running.

On wide screens, the form and monitor scroll independently within a fixed
page layout. The monitor output follows the newest line while **Follow** is
ticked. Untick it to scroll back without being yanked forward. At widths of
880 pixels or less, the page uses normal document scrolling.

The monitor takes every pixel the form does not need. Drag the grey handle
between the two parts to overrule that. A dragged height holds while you stay
on that page.

### Adding an engine

Click **+** next to ENGINES:

| Field                    | Meaning                                                                                             |
|--------------------------|-----------------------------------------------------------------------------------------------------|
| **Name**                 | Shown in the left pane; must be unique.                                                             |
| **Port**                 | The port the engine listens on. llm-pivot proxies to `127.0.0.1:<port>`. Must differ from the monitor and router ports. |
| **Command**              | Program and default options, e.g. `/path/llama-server -ctx 64000`. Quoting is shell-like, but **no shell is involved**: no globbing, `~`, `$VAR`, or pipes. |
| **Monitoring info from** | Where the right pane gets its content — see [Monitoring modes](#monitoring-modes).                  |
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
| **Model name**                | What OpenAI clients put in `"model"`. **Must be unique across every engine.**       |
| **Additional engine options** | Extra flags for this model. A repeated flag **replaces** the engine's value for it. |

The **Effective command line** box shows exactly what will run after the merge.

### Monitoring modes

| `monitor:`          | The right pane shows                                | Use when                                   |
|---------------------|-----------------------------------------------------|--------------------------------------------|
| `stdio` *(default)* | the engine's stdout/stderr, live                    | the engine logs what you care about        |
| `http`              | the body of `monitor_url`, re-fetched each interval | the engine exposes metrics as text or JSON |
| `embed`             | `monitor_url` itself, rendered in a frame           | the engine has its own monitoring page     |

`http` and `embed` get a second tab, **Process output**. `embed` also gets a
**↻** button and **Open ↗**.

**A `#fragment` only works with `embed`.** `http` mode fetches the URL
server-side, and a fragment never reaches a server. An engine that sends
`X-Frame-Options: DENY` cannot be framed either — use `http` or **Open ↗**.

**The framed page is proxied, never loaded directly.** `monitor_url` names
the engine as llm-pivot reaches it, on llm-pivot's own loopback. A browser
given that address resolves it against the machine *the browser* runs on, so
viewing the page from anywhere else framed the wrong host. llm-pivot fetches
the page itself and serves it at `/engine/` on the monitor port, which is the
same origin the page was loaded from and therefore always reachable. Only the
monitor port has to be reachable — the engine can stay on loopback, and
`monitor_url` may name any address the llm-pivot machine can reach.

The frame waits for the engine to answer. An engine loading a model binds
its port long after its process exists, so until the state is `running` the
frame says it is still starting rather than showing a dial error — the
**Process output** tab is where the load itself is visible.

Everything below `/engine/` is forwarded, so a page built from relative links
works unchanged. A page that loads sub-resources by absolute path (`/app.js`)
does not: those escape the prefix and hit llm-pivot's own page instead.

```yaml
monitor: embed
monitor_url: http://127.0.0.1:{{port}}/#monitor
```

### Starting, stopping, restarting

On a model page:

- **Start** — kills any other engine first, then starts this one and waits for it to answer.
- **Restart** — kills and starts the same model.
- **Stop** — kills the running engine.
- **Copy model** — adds `<name>-copy` with the options copied. Nothing starts or stops. Copying twice is refused.

Changing the active engine’s command, port, monitoring mode/URL, health path,
port flag, working directory or environment stops it. Renaming or deleting
the active engine/model, or changing the active model’s options, also stops
it; start it again afterwards. Saving unchanged values or editing another
model leaves it running. Startup timeout changes apply on the next start;
HTTP polling interval changes also take effect on the next start.

An engine also stops by itself once `inactive_stop_timeout_seconds` passes with
no proxied request, freeing the VRAM. Only proxied requests keep it alive;
`/v1/models` and `/router/status` do not. A request in flight always counts.
Set `-1` to disable automatic idle stopping. Inactivity is checked every
30 seconds, so stopping can occur up to about 30 seconds after the timeout.
An omitted or zero timeout uses the default of 600 seconds.

### Settings

Monitor port, router port, bind address, the three timeouts and the log buffer
size. Saving a changed address or port reopens the affected listeners after
the response; the running engine is untouched. Moving a listener can interrupt
requests still using the old address after its three-second shutdown grace.

> Bind address defaults to `127.0.0.1`. Use `0.0.0.0` only if you want the
> page and router reachable from the network — neither has authentication.

---

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

What happens:

1. Read the `"model"` field.
2. Already running → forwarded as is.
3. Different model running → its engine is killed, the right engine starts and
   becomes healthy, then the request is forwarded.
4. Not configured → `404`, OpenAI-shaped error; the running engine is left alone.

The request path and body are forwarded without rewriting the configured
model name, and streaming responses are flushed immediately. The engine must
accept that model name itself if it validates the `"model"` field.

Requests without a model go to the current engine. GET/HEAD bodies and bodies
with a non-JSON Content-Type (such as multipart uploads) are not inspected.
The `?model=<name>` query parameter takes precedence over a JSON model field.

A request for a different model stops the current engine even if it has
requests in flight, so concurrent clients requesting different models can
interrupt one another.

Finding the `"model"` field means buffering the request body, which is capped
at 64 MiB. A bigger body (inline base64 audio or images) is forwarded whole
and untouched, but goes to whatever engine is running rather than switching
models. Put `?model=<name>` in the query string to route it without the body
being read at all.

Two endpoints are answered by llm-pivot itself:

| Endpoint             | Answer                                                                   |
|----------------------|--------------------------------------------------------------------------|
| `GET /v1/models`     | Every configured model, each with `owned_by` (the engine) and `running`. |
| `GET /router/status` | The runtime status the monitor page shows.                               |

### Who answered? (`X-Llm-Pivot*`)

Every reply carries headers saying where it came from:

| Header                 | On                        | Meaning                                         |
|------------------------|---------------------------|-------------------------------------------------|
| `X-Llm-Pivot`          | every reply               | the version                                     |
| `X-Llm-Pivot-Upstream` | **proxied replies only**  | `<engine>/<model>:<port>` — the engine answered |

**If `X-Llm-Pivot-Upstream` is present, the engine answered; if absent,
llm-pivot answered.**

```sh
$ curl -sD- -o/dev/null localhost:8078/chat/completions -d '{"model":"my-model"}'
HTTP/1.1 404 Not Found
X-Llm-Pivot: 1.0.0
X-Llm-Pivot-Upstream: strata/my-model:8024     ← the engine said no
```

Engine error statuses are written to the monitoring pane, so the page shows
what a client is doing wrong. Repeats of the same status/method/path are
suppressed for 10 seconds.

llm-pivot's own error payloads follow the OpenAI shape:

| Code                  | HTTP| When                                     |
|-----------------------|-----|------------------------------------------|
| `invalid_request_error` | 400 | the request body could not be read       |
| `model_not_found`     | 404 | the model is not configured              |
| `engine_start_failed` | 503 | the engine would not start               |
| `no_engine_running`   | 503 | nothing runs and no model named          |
| `engine_starting`     | 503 | the engine is still coming up            |
| `engine_unavailable`  | 502 | the engine stopped answering mid-request |

---

## Configuration file

Default location, by precedence:

1. `-config <path>`
2. `$LLM_PIVOT_CONFIG`
3. `$XDG_CONFIG_HOME/llm-pivot/config.yaml`
4. `~/.config/llm-pivot/config.yaml`

The page writes this file on every change. Hand-written comments are not
preserved once you save from the page.

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

**Top level**

| Key                             | Default     | Meaning                                    |
|---------------------------------|-------------|--------------------------------------------|
| `monitor_port`                  | `8061`      | web page port                              |
| `router_port`                   | `8078`      | OpenAI port                                |
| `host`                          | `127.0.0.1` | bind address for both ports                |
| `startup_timeout_seconds`       | `180`       | wait for TCP and optional health probe                      |
| `graceful_stop_timeout_seconds` | `20`        | `SIGTERM` grace before `SIGKILL`           |
| `inactive_stop_timeout_seconds` | `600`       | stop an idle engine after this; `-1` never |
| `log_lines`                     | `4000`      | monitoring buffer size; resized live       |

**Per engine**

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

**Per model**

| Key       | Meaning                                                       |
|-----------|---------------------------------------------------------------|
| `name`    | unique across **all** engines; what clients send as `"model"` |
| `options` | extra flags; override the engine's own flags                  |

### Placeholders

Usable in `command`, `options`, `monitor_url`, `work_dir`, `env`:
`{{port}}`, `{{engine}}`, `{{model}}`/`{{name}}`, `{{host}}`, `{{home}}`.

Use `{{home}}/models/model.gguf` instead of `~/models/model.gguf`; shell
expansion does not occur.

`{{host}}` is always `127.0.0.1`.  An engine has to be reachable on
the loopback interface to be proxied to at all, so `--host {{host}}` is the
safe way to write that.

### Startup validation

llm-pivot refuses to start on: unparseable YAML or an unknown option, a
duplicated or empty engine/model name, a bad or conflicting listener port, an
empty or unparseable `command`, malformed model options, an unknown monitoring
mode, `http` or `embed` with no `monitor_url`, a nonempty `health_path` without
a leading `/`, or an `env` entry that is not `KEY=VALUE`. Engine ports may be
shared across engines, since only one runs at a time. Exit status is `1` for
validation failures. Validation does not check whether executable or model
files exist; those problems surface when starting or using the engine.

---

## How engine and model options are merged

The engine's `command` carries the defaults; a model's `options` layer on top:

- a flag in **both** keeps the engine's **position** but takes the **model's
  value**; repeats of it in the engine command are dropped;
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

You cannot *remove* an engine flag from a model; give it the value you want
(`--flash-attn off`). The merged line is shown on each model page and written
to the monitor as `$ …` when an engine starts.

---

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

Overrides are not written back to the file. They belong to the run, not to
the configuration: saving anything from the page writes the file's own
`monitor_port`, `router_port` and `host` back unchanged, and a reload
(`systemctl --user reload llm-pivot`) puts the overrides on again after
re-reading the file, so the listeners stay where the command line asked.

The page shows the value in force, which is the override. Typing a *different*
one into the settings form does take effect and is written to the file — the
page never silently ignores what you type — and the form says which override
it had to drop to do so. The next start applies the flag again.

`-check` validates the file on its own and then once more with the overrides
applied, so a flag that collides with an engine port is reported too.

---

## HTTP API

Everything the page does is a plain JSON call on the monitor port, so it can be
scripted.

| Method & path                                 | Body / result                                              |
|-----------------------------------------------|------------------------------------------------------------|
| `GET /api/state?since=<seq>`                  | configuration, status, log lines newer than `seq`          |
| `GET /api/state?…&monitor_body=1`             | the same, plus the sampled body of an `http` monitor       |
| `GET /api/command?model=<name>`               | the effective command line                                 |
| `PUT /api/settings`                           | `{monitor_port, router_port, host, …timeouts…, log_lines}` |
| `POST /api/engines`                           | engine fields plus `original` (empty = create)             |
| `DELETE /api/engines/{engine}`                | removes the engine and its models                          |
| `POST /api/models`                            | `{engine, original, name, options}`                        |
| `DELETE /api/engines/{engine}/models/{model}` |                                                            |
| `POST /api/control`                           | `{"action":"start"\|"stop"\|"restart","model":"<name>"}`   |
| `POST /api/log/clear`                         | empties the monitoring buffer                              |
| `GET /engine/…`                               | the running `embed` engine's own page, proxied             |

API rejections come back as `{"error":"…"}` with a 4xx status. JSON request
bodies are limited to 1 MiB. Engine/model saves replace their editable fields,
so send the complete definition; `original` identifies the existing name
when updating or renaming. Configuration edits return `notes`, `ports_moved`,
`monitor_port` and `router_port`.

`/api/state` also returns `log_seq` and `log_gap`. Use `log_seq` as the next
`since` value; `log_gap` means older requested lines were cleared or dropped
from the buffer.

The page is a single page, deep-linkable: `#settings`, `#engine/<name>`,
`#model/<engine>/<model>`.

---

## Troubleshooting

**Service will not start.** Run the binary by hand; the reason is on stderr and
in `journalctl --user -u llm-pivot`.

**Engine will not start.** Its output is in the monitor pane, under the `$ …`
line. Usual causes: wrong path, unknown option, missing model file.

**"did not answer on port N".** The engine started but never served
`health_path`. It may need more time, listen on a different port, or bind an
address other than loopback.

**"exited during startup".** The process died; exit status and last output are
in the pane above the message.

**"port N is already in use".** Something else listens there — usually an engine
you started earlier. llm-pivot refuses to start on top of it on purpose. Find it
with `ss -ltnp 'sport = :N'`.

**A browser window opens on start.** That is the engine, not llm-pivot. Drop the
flag that opens the page (strata's `--open`, for instance), or point
`monitor: embed` at it.

**Monitor shows raw HTML.** You are in `http` mode. Switch the engine to
`embed`.

**Big models: know which wait you are looking at.** Only the first is governed
by the startup timeout:

| Wait                                       | Covered by                | Typical        |
|--------------------------------------------|---------------------------|----------------|
| process start → port open and health probe succeeds | `startup_timeout_seconds` | seconds to minutes |
| first request → first token (weights load) | nothing                   | can be minutes |

Many engines open the port immediately and load lazily on the first request.
llm-pivot sets no deadline on the second wait. **Your client's timeout is the
one that matters**, and a client that gives up mid-load cancels the request.
Raise the client timeout, or send a warm-up request through the router.
Starting a model from the monitor page only waits for the configured readiness
probe; it does not send a generation request.

**The port will not change.** If the new port is taken, the rebind fails and the
old port keeps working. The new setting is still saved, so correct it in the
configuration before restarting the service.

**Model names and the router.** The router dispatches purely on the configured
model *name*, which is why names must be unique across engines. The name need
not match the model filename, but the engine must accept it in forwarded
requests (configure an engine-side alias if needed).

**Nothing proxied / 503.** Either no engine is running and no model was named,
or the engine is still loading. `GET /router/status` says which.

**404 from a client, but the engine runs.** Check `X-Llm-Pivot-Upstream`. If it
is present, the engine rejected the *path*, not the model — usually a client
base URL missing `/v1`:

```
http://127.0.0.1:8078/v1   ✅
http://127.0.0.1:8078      ❌  →  POST /chat/completions  →  404
```

Ask the engine directly to see what it serves:

```sh
curl -s localhost:<engine port>/v1/models
```
