# llm-pivot

A small Go server that **starts, stops and monitors local LLM engines**
(llama.cpp, strata, ninfer, …) and **routes OpenAI-compatible traffic** to
whichever engine is serving the requested model.

Two ports:

| Port             | Default | What it is                                                   |
|------------------|---------|--------------------------------------------------------------|
| **monitor port** | `8061`  | The web page: configure engines/models, start/stop, watch output |
| **router port**  | `8078`  | OpenAI-compatible endpoint, forwarded to the running engine   |

**Exactly one engine with one model runs at a time.** Starting a model — from
the page or implicitly, by an OpenAI request naming it — kills whatever was
running before.

llm-pivot is mostly written by Claude Opus 5.

---

## Contents

- [Why](#why)
- [Install](#install)
- [Running under systemd](#running-under-systemd---user)
- [The monitor page](#the-monitor-page)
- [Routing OpenAI requests](#routing-openai-requests)
- [Configuration file](#configuration-file)
- [How engine and model options are merged](#how-engine-and-model-options-are-merged)
- [Command-line flags](#command-line-flags)
- [HTTP API](#http-api)
- [Troubleshooting](#troubleshooting)

---

## Why

I work with many different local models, and they do not all run on the same
engine: some want llama.cpp, some want strata or ninfer, some want a
particular llama.cpp clone such as prism-llama. Only one of them can hold the
VRAM at a time, so every switch meant stopping the engine that had it and
starting the next one by hand, with the right flags. Watching them was no
better — each engine reports where it likes, so its logs or its HTTP status
page were never in the same place twice.

llm-pivot is the minimal router that solves both problems for me. One
OpenAI-compatible port starts whichever engine serves the model a request
names, freeing the VRAM first, and one page shows the output or status page of
whatever is running. I hope it is useful to other people with the same
problems.

llm-pivot is mostly written by Claude Opus 5.

---

## Install

Requires Go 1.22 or newer (developed against 1.26).

```sh
cd ~/GITHUB/llm-pivot
go build -o llm-pivot .
```

The web page is compiled into the binary, so the single executable is all you
need to deploy.

First start writes a default configuration if none exists:

```sh
./llm-pivot
# llm-pivot: created default configuration /home/you/.config/llm-pivot/config.yaml
# llm-pivot: monitor: listening on http://127.0.0.1:8061
# llm-pivot: router:  listening on http://127.0.0.1:8078
```

Open <http://127.0.0.1:8061>.

**Example configuration:** See `config.example.yaml` in this repo for a template showing
how to configure llama.cpp, strata, and other engines with real model paths:

```yaml
command: llama-server ...           # path to llama-serve binary
work_dir: ~/Strata                  # working directory for strata
models:
  - options: -m ~/models/model.gguf # model file path
```

Check a configuration without starting anything:

```sh
./llm-pivot -check
./llm-pivot -check -config /path/to/other.yaml
```

---

## Running under systemd (`--user`)

```sh
cp `pwd`/llm-pivot.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now llm-pivot
```

Everyday commands:

```sh
systemctl --user status  llm-pivot
systemctl --user restart llm-pivot
systemctl --user reload  llm-pivot     # re-read the YAML file (SIGHUP)
systemctl --user stop    llm-pivot
journalctl --user -u llm-pivot -f
```

To keep the service running when you are not logged in:

```sh
loginctl enable-linger $USER
```

The shipped unit assumes the binary lives at
`~/GITHUB/llm-pivot/llm-pivot` and the configuration at
`~/.config/llm-pivot/config.yaml`; edit `ExecStart` if yours differ.

On stop, llm-pivot sends `SIGTERM` to the engine's whole process group,
waits `graceful_stop_timeout_seconds`, then `SIGKILL`s it. **No engine is ever
left behind.**

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
│     + Add model    │  Effective command line                             │
│ ▸ strata    :8090  │  /path/llama-server … -m /models/… --port 8080      │
│     ○ phi4-mini    │ [Save model] [Restart] [Stop] [Copy model] [Delete] │
│     + Add model    │ ─────────────────────────────────────────────────── │
│                    │  Engine monitor · llama.cpp · qwen3-30b             │
│ ~/.config/…yaml    │  ┌───────────────────────────────────────────────┐  │
│                    │  │ 12:05:51 ✓ engine "llama.cpp" is serving …    │  │
│                    │  │ 12:05:53 llama_model_loader: loaded …         │  │
│                    │  └───────────────────────────────────────────────┘  │
└────────────────────┴─────────────────────────────────────────────────────┘
          the ───── between the panes is the drag handle
```

### Left pane

Engines in bold with their port; each engine's models are listed **indented
below it**. A coloured light shows the run state of a model:

| Light | Meaning |
|---|---|
| ● green | running and answering |
| ● amber (pulsing) | starting |
| ● amber | stopping |
| ● red | the engine failed or died |
| ○ grey | not running |

Click anything to open it in the right pane: **Settings**, an **engine**, a
**model**, **+ (add engine)** or **Add model**.

### Right pane

The right pane always has two parts:

1. **The thing you clicked** — its settings, and the buttons that act on it.
   This part scrolls if the form is taller than the space available.
2. **Engine monitor** — the live monitoring output of the engine that is
   running right now, whatever is selected above.

The page itself never scrolls: the monitor pane is **always fully visible**
at the bottom of the window, and the output **slides inside it**, following
the newest line while **Follow** is ticked. Untick **Follow** to scroll back
through the buffer without being yanked forward.

The monitor takes **every pixel the form above does not need**, so it is as
tall as the selected engine or model allows and grows again when you select
a shorter page. A form taller than the window keeps the monitor at its
minimum and scrolls instead.

Drag the **grey handle** between the two parts to overrule that (or focus it
with Tab and use ↑/↓, Shift for bigger steps). A dragged height holds while
you stay on that page; selecting another engine or model fits the monitor to
the new form again.

### Adding an engine

Click **+** next to ENGINES and fill in:

| Field | Meaning |
|---|---|
| **Name** | Shown in the left pane; must be unique. |
| **Port** | The port the engine listens on. llm-pivot proxies to `127.0.0.1:<port>`. Must differ from the monitor and router ports. |
| **Command** | Program and its default options, e.g. `/path/llama-server -ctx 64000`. The box grows to show the whole command. Quoting is shell-like, but **no shell is involved**: no globbing, no `$VAR`, no pipes. |
| **Monitoring info from** | How the right pane gets its content — see [Monitoring modes](#monitoring-modes). |
| **Monitor URL** | For the `http` and `embed` modes, e.g. `http://127.0.0.1:{{port}}/metrics`. |
| **Poll interval** | How often that URL is sampled (seconds). `http` mode only. |
| **Health path** | Polled while starting, until it answers 2xx/3xx. Empty means "an open TCP port is enough". |
| **Port flag** | Appended with the engine port unless the command already sets it (default `--port`). Use `-` to never inject it. |
| **Startup timeout** | Seconds to wait for the engine's **port to open**; `0` uses the global setting. It does *not* cover a model that loads lazily on the first request — see [Troubleshooting](#troubleshooting). |
| **Working directory** | Optional `cwd` for the process. |
| **Environment** | Extra `KEY=VALUE` lines added to the engine's environment. |

### Adding a model

Click **Add model** under an engine:

| Field | Meaning |
|---|---|
| **Model name** | What OpenAI clients put in `"model"`. **Must be unique across every engine** — the page and the loader both refuse duplicates. |
| **Additional engine options** | Extra flags for this model. The box grows to show them all. A flag repeated here **replaces** the engine's value for it. |

The **Effective command line** box shows exactly what will be executed after
the merge, including the injected port.

### Monitoring modes

| `monitor:` | The right pane shows | Use when |
|---|---|---|
| `stdio` *(default)* | the engine's stdout/stderr, live | the engine logs what you care about |
| `http` | the body of `monitor_url`, re-fetched every `monitor_interval_seconds` | the engine exposes metrics as text or JSON |
| `embed` | `monitor_url` itself, rendered in a frame | the engine has its own monitoring **web page** |

With `http` and `embed` the pane gets a second tab, **Process output**, so the
engine's stdout/stderr is always one click away. `embed` also gets a **↻**
button and **Open ↗**, since the pane is small.

**A `#fragment` only works with `embed`.** `http` mode fetches the URL
server-side, and a fragment is never sent to a server — `/#monitor` fetches
plain `/` and you get the page's HTML source as text. A fragment that selects
a tab in the engine's own single-page app only does anything in a real
browser, which is what `embed` gives it:

```yaml
    monitor: embed
    monitor_url: http://127.0.0.1:{{port}}/#monitor
```

The frame is pointed at the URL once and left alone; the page refreshes
itself. llm-pivot's own once-a-second poll does **not** reload it.

An engine that sends `X-Frame-Options: DENY` or a CSP `frame-ancestors`
cannot be framed — the pane will be blank. Use `http` mode against one of its
JSON endpoints instead, or **Open ↗**.

### Starting, stopping, restarting

On a model page:

- **Start** — appears when the model is not running. Kills any other engine
  first, then starts this one and waits for it to answer.
- **Restart** — kills and starts the same model.
- **Stop** — kills the running engine; nothing is serving afterwards.
- **Copy model** — adds `<model name>-copy` to the same engine with the saved
  options copied verbatim, and opens it. Nothing is started or stopped.
  Copying twice is refused: the second `-copy` would duplicate the first.

Editing a running engine or its model **stops it**, with a note explaining
why; start it again when you are done editing.

An engine also stops **by itself** once `inactive_stop_timeout_seconds` passes
without a request being proxied to it, freeing the VRAM; the monitoring output
says why. Only proxied requests count: `/v1/models`, `/router/status` and
watching the monitor page do **not** keep an engine alive. A request in flight
always does, however long the generation takes — a slow reply is never cut off
by this timeout. Set it to `-1` to keep an engine running forever.

### Settings

Monitor port, router port, bind address, startup timeout, graceful stop timeout,
inactive stop timeout, and the log buffer size. Saving reopens the listeners
immediately — no restart. If you changed the monitor port the page moves itself
to the new address. The running engine is **not** disturbed by a port change.

**Timeouts:**
- **Startup timeout** — how long to wait for a freshly started engine to answer
- **Graceful stop timeout** — grace period for a stopping engine before killing it
- **Inactive stop timeout** — stop the engine after this long without a single
  routed request (`-1` keeps it running forever)

> The bind address defaults to `127.0.0.1`. Set it to `0.0.0.0` only if you
> want the page and the router reachable from the network — neither has any
> authentication.

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

1. The `"model"` field is read from the request.
2. If that model is **already running**, the request is forwarded as is.
3. If a **different** model is running, its engine is **killed**, the right
   engine is started with the right options, llm-pivot waits for it to
   become healthy, and only then forwards the request.
4. If the model is **not configured**, the request fails with `404` and an
   OpenAI-shaped error — the running engine is left alone.

Everything else is proxied verbatim to the running engine, including
streaming responses (`"stream": true` is flushed through immediately).

Two endpoints are answered by llm-pivot itself:

| Endpoint | Answer |
|---|---|
| `GET /v1/models` | Every configured model from every engine, each with `owned_by` (the engine) and `running`. Clients can therefore discover models that are not loaded yet. |
| `GET /router/status` | The same runtime status the monitor page shows. |

### Who answered? (`X-Llm-Pivot*`)

Every reply on the router port carries headers saying where it came from, so
an error is never ambiguous:

| Header | On | Meaning |
|---|---|---|
| `X-Llm-Pivot` | every reply | the version; you really are talking to llm-pivot |
| `X-Llm-Pivot-Upstream` | **proxied replies only** | `<engine>/<model>:<port>` — the engine produced this |

**If `X-Llm-Pivot-Upstream` is present, the engine answered; if it is
absent, llm-pivot answered.** A 404 with the header is your client asking
the engine for a path it does not serve; a 404 without it is a model
llm-pivot does not know.

```sh
$ curl -sD- -o/dev/null localhost:8078/chat/completions -d '{"model":"my-model"}'
HTTP/1.1 404 Not Found
X-Llm-Pivot: 1.0.0
X-Llm-Pivot-Upstream: strata/my-model:8024     ← the engine said no
```

Error statuses coming back from the engine are also written to the
monitoring pane, so the page shows what a client is doing wrong:

```
engine "strata" answered 404 to POST /chat/completions (the engine rejected
it, not llm-pivot); "strata" has no "/chat/completions" — most OpenAI
engines only serve /v1/…, so check that your client's base URL ends in /v1
```

Repeats of the same status/method/path are suppressed for 10 seconds, so a
client in a retry loop cannot flood the buffer.

Error payloads **generated by llm-pivot** follow the OpenAI shape:

```json
{"error":{"message":"model \"ghost\" is not configured in llm-pivot; …",
          "type":"model_not_found","param":null,"code":"model_not_found"}}
```

| Code | HTTP | When |
|---|---|---|
| `model_not_found` | 404 | the model is not in the configuration |
| `engine_start_failed` | 503 | the engine would not start (reason included) |
| `no_engine_running` | 503 | nothing runs and the request names no model |
| `engine_starting` | 503 | the engine is still coming up |
| `engine_unavailable` | 502 | the engine stopped answering mid-request |

---

## Configuration file

Default location, in order of precedence:

1. `-config <path>`
2. `$LLM_PIVOT_CONFIG`
3. `$XDG_CONFIG_HOME/llm-pivot/config.yaml`
4. `~/.config/llm-pivot/config.yaml`

The page writes this file back on every change. **Hand-written comments are
not preserved** once you save from the page.

```yaml
monitor_port: 8061                    # the web page
router_port: 8078                     # OpenAI-compatible port
host: 127.0.0.1                       # bind address for both
startup_timeout_seconds: 180          # how long an engine may take to answer
graceful_stop_timeout_seconds: 20     # SIGTERM grace period before SIGKILL
inactive_stop_timeout_seconds: 600    # stop an idle engine after this long; -1 never
log_lines: 4000                       # monitoring buffer size

engines:
  - name: llama.cpp
    port: 8080
    command: >-
      /home/vmakarov/GITHUB/llama.cpp/build/bin/llama-server
      --models-max 1
      --sleep-idle-seconds 600
      --models-autoload true
      --jinja on
      --flash-attn on
      --cache-type-k q8_0
      --cache-type-v q4_0
      --n-gpu-layers 99
      --ubatch-size 512
      --batch-size 1024
      --reasoning-format deepseek
      --reasoning-effort medium
      --reasoning-budget 2040
      --reasoning-budget-message "Thinking time expired. Formulate your final response now."
      --device Vulkan1
      --sse-ping-interval 30
      --fit off
      --timeout 600
    monitor: stdio            # or: http
    health_path: /health
    models:
      - name: qwen3-30b
        options: -m /models/Qwen3-30B-A3B-Q4_K_M.gguf
      - name: gemma3-27b
        options: -m /models/gemma-3-27b-it-Q4_K_M.gguf --ctx-size 32768
```

### Keys

**Top level**

| Key | Default | Meaning |
|---|---|---|
| `monitor_port` | `8061` | web page port |
| `router_port` | `8078` | OpenAI port; must differ from `monitor_port` |
| `host` | `127.0.0.1` | bind address for both ports |
| `startup_timeout_seconds` | `180` | global startup budget |
| `graceful_stop_timeout_seconds` | `20` | `SIGTERM` grace before `SIGKILL` |
| `inactive_stop_timeout_seconds` | `600` | stop an idle engine after this long; `-1` never does |
| `log_lines` | `4000` | lines kept in the monitoring buffer |
| `engines` | — | list of engines |

**Per engine**

| Key | Default | Meaning |
|---|---|---|
| `name` | required | unique engine name |
| `port` | required | where the engine listens |
| `command` | required | program and default options |
| `monitor` | `stdio` | `stdio`, `http` or `embed` — see [Monitoring modes](#monitoring-modes). `stderr`/`stdout`/`logs` mean `stdio`; `iframe`/`frame`/`page`/`web` mean `embed` |
| `monitor_url` | — | required when `monitor` is `http` or `embed` |
| `monitor_interval_seconds` | `5` | http polling interval (`http` mode only) |
| `health_path` | — | startup probe path; empty = TCP connect only |
| `port_flag` | `--port` | injected with the port unless already present; `-` disables |
| `startup_timeout_seconds` | — | overrides the global value |
| `work_dir` | — | working directory of the process |
| `env` | — | list of `KEY=VALUE` |
| `models` | — | list of models |

**Per model**

| Key | Meaning |
|---|---|
| `name` | unique across **all** engines; what clients send as `"model"` |
| `options` | extra flags; override the engine's own flags |

### Placeholders

Usable in `command`, `options`, `monitor_url`, `work_dir` and `env`:

| Placeholder | Expands to |
|---|---|
| `{{port}}` | the engine's port |
| `{{engine}}` | the engine name |
| `{{model}}` / `{{name}}` | the model name |
| `{{home}}` | `$HOME` |

### Common paths

**llama-serve** (llama.cpp):
```yaml
command: /path/to/llama.cpp/build/bin/llama-server
```
Update `/path/to` to wherever you cloned and built `llama.cpp`.

**strata**:
```yaml
work_dir: ~/Strata                         # strata's working directory
command: ~/Strata/.venv/bin/python ~/Strata/serve/server.py
```

**Models**:
```yaml
options: -m ~/models/Qwen3-30B-Q4_K_M.gguf  # llama.cpp format
options: --config ~/models/swift-iq2_xs.json # strata format
```
Store models in `~/models/` or any path you have space; adjust the `-m` or config path accordingly.

### Startup validation

The program **refuses to start** and prints the reason for any of:

- YAML that does not parse, or an **unknown option**
  → `line 5: field turbo not found in type main.Engine`
- a **duplicated model name**, in one engine or across two
  → `duplicated model name "dup": used by engine "a" and engine "b" (model names must be unique across all engines)`
- a duplicated or empty engine name
- a port outside 1–65535, `monitor_port == router_port`, or an engine on
  either of those ports
- an empty or unparseable `command` (e.g. an unterminated quote)
- `monitor: http` with no `monitor_url`, or an unknown `monitor` value
- an `env` entry that is not `KEY=VALUE`

The exit status is `1` in every case.

---

## How engine and model options are merged

The engine's `command` carries the defaults; a model's `options` are layered
on top:

- a flag that appears in **both** keeps the engine's **position** but takes
  the **model's value**, and any repeat of that flag in the engine command is
  dropped;
- a flag only the model has is **appended**;
- bare operands from the model are appended;
- `--flag=value` and `--flag value` are the same flag;
- `-1` after a flag is a value, not a flag (`--seed -1` survives intact);
- finally `--port <engine port>` is appended unless the command already sets
  it (see `port_flag`).

```
engine command : /bin/llama-server --ctx-size 64000 --device Vulkan1 --flash-attn on
model options  :                   --ctx-size 8192  -m /models/x.gguf
──────────────────────────────────────────────────────────────────────────────────
executed       : /bin/llama-server --ctx-size 8192  --device Vulkan1 --flash-attn on \
                                   -m /models/x.gguf --port 8080
```

There is no way to *remove* an engine flag from a model; give it the value
you want instead (`--flash-attn off`).

The **Effective command line** box on each model page shows the result, and
the same line is written to the monitoring output as `$ …` whenever an engine
starts.

---

## Command-line flags

```
-config string         path of the YAML configuration file
                       (default ~/.config/llm-pivot/config.yaml)
-monitor-port int      override monitor_port from the configuration
-router-port int       override router_port from the configuration
-host string           override the bind address
-init                  write a default configuration file and exit
-check                 validate the configuration file and exit
-version               print the version and exit
```

Overrides are **not** written back to the file; the page saves them if you
then change something.

---

## HTTP API

Everything the page does is a plain JSON call on the monitor port, so it can
be scripted.

| Method & path | Body / result |
|---|---|
| `GET /api/state?since=<seq>` | configuration, runtime status, and log lines newer than `seq` |
| `GET /api/command?model=<name>` | the effective command line for a model |
| `PUT /api/settings` | `{monitor_port, router_port, host, startup_timeout_seconds, graceful_stop_timeout_seconds, inactive_stop_timeout_seconds, log_lines}` |
| `POST /api/engines` | engine fields plus `original` (empty = create, otherwise the name being edited) |
| `DELETE /api/engines/{engine}` | removes the engine and its models |
| `POST /api/models` | `{engine, original, name, options}` |
| `DELETE /api/engines/{engine}/models/{model}` | |
| `POST /api/control` | `{"action":"start"\|"stop"\|"restart","model":"<name>"}` |
| `POST /api/log/clear` | empties the monitoring buffer |

Rejections come back as `{"error":"…"}` with a 4xx status, and nothing is
written to the configuration file.

```sh
# start a model from a script
curl -s -X POST localhost:8061/api/control \
     -H 'Content-Type: application/json' \
     -d '{"action":"start","model":"qwen3-30b"}'

# what is running?
curl -s localhost:8078/router/status
```

The page is a deep-linkable single page: `#settings`,
`#engine/<name>`, `#model/<engine>/<model>`.

---

## Troubleshooting

**The service will not start.** Run the binary by hand — the reason is
printed on stderr and in `journalctl --user -u llm-pivot`:

```sh
~/GITHUB/llm-pivot/llm-pivot -check
```

**An engine will not start.** Its stdout and stderr are in the monitor pane,
right under the `$ …` line showing the exact command. The usual causes are a
wrong path in `command`, an option the engine does not know, or a model file
that is not there.

**"did not answer on port N within …".** The engine started but never served
`health_path`. Either it needs longer (raise **Startup timeout**), it listens
on a different port than the engine's `port` (check what you passed as
`--port`, or set `port_flag`), or it binds an address other than loopback —
llm-pivot always connects to `127.0.0.1`.

**"exited during startup".** The process died; the exit status and its last
output are in the pane above the message.

**"port N is already in use by another process".** Something is already
listening there — usually an engine you started by hand earlier, or a
previous llm-pivot. llm-pivot refuses to start on top of it on
purpose: the stranger would answer the health probe, the start would look
successful, and your requests would be served by the wrong model. Find it
with `ss -ltnp 'sport = :N'`, stop it, and start again.

**A browser window opens whenever the engine starts.** That is the engine,
not llm-pivot — several of them take a flag that pops the local page open
when they are ready (strata's `--open`, for instance). Drop that flag from
the engine's **Command**; llm-pivot never opens a browser itself. To keep
an eye on that page, point `monitor: embed` at it and it appears in the
monitor pane instead.

**The monitor pane shows raw HTML, or a page fragment is ignored.** You are
in `http` mode, which fetches the URL server-side. Switch the engine to
`embed` — see [Monitoring modes](#monitoring-modes).

**Big models: know which wait you are looking at.** There are two very
different delays, and only the first is governed by the startup timeout:

| Wait | Covered by | Typical |
|---|---|---|
| process start → port open | `startup_timeout_seconds` | seconds |
| first request → first token (weights load) | **nothing in llm-pivot** | can be minutes |

Many engines open their port immediately and load the model lazily on the
first request. Measured on a real strata engine: port open in **0.47 s**,
first chat request **45 s**, second request **0.12 s**. For that engine the
startup timeout is never the constraint.

llm-pivot does not impose any deadline on the second wait — there is no
write timeout on the router port and no response-header timeout on the proxy
transport, so it will wait as long as the engine takes. **Your client's
timeout is the one that matters.** Worse, if the client gives up mid-load the
proxy cancels the upstream request, so a client with a 30 s timeout can
cancel the load over and over and never succeed. Raise the timeout in the
client, or warm the model once from the monitor page before pointing the
client at it.

If you do have an engine that loads everything *before* opening its port,
give it room with its own **Startup timeout** on the engine page
(`startup_timeout_seconds`), which overrides the global value.

**The port will not change.** If the new port is already taken the rebind
fails, a note is written to the monitoring output, and the old port keeps
working. Pick another port.

**Model names and the router.** The router dispatches purely on the model
*name* you configured, which is why names must be unique across engines. The
name need not match anything inside the model file — it is the handle clients
use.

**Nothing is proxied / 503.** Either no engine is running and the request did
not name a model, or the engine is still loading. `GET /router/status` says
which.

**404 from a client, but the engine is running.** Check for
`X-Llm-Pivot-Upstream` on the reply (see
[Who answered?](#who-answered-x-llm-pivot)). If it is there, the engine
rejected the *path*, not the model — the usual cause is a client base URL
without `/v1`:

```
http://127.0.0.1:8078/v1      ✅
http://127.0.0.1:8078         ❌  →  POST /chat/completions  →  404
```

Clients written against an API whose host accepts both forms (DeepSeek's
`https://api.deepseek.com` works with and without `/v1`) often default to
the bare host. Most local engines only mount `/v1/…`. It is doubly
confusing when an engine registers `/models` *and* `/v1/models` but only
`/v1/chat/completions`: discovery succeeds, then the chat call 404s.

To see what an engine really serves, ask it directly rather than through the
router:

```sh
curl -s localhost:<engine port>/v1/models          # does /v1 work?
curl -s -o/dev/null -w '%{http_code}\n' -XPOST localhost:<engine port>/chat/completions -d '{}'
```

Also check the endpoint *kind*: an engine may serve `/v1/chat/completions`
but not the legacy `/v1/completions` or a FIM endpoint, in which case fixing
the base URL will not help.

---

## Design notes

- **One engine, one model, ever.** Every start path funnels through a single
  lock that stops the previous process first, so a burst of requests for
  different models cannot spawn two engines.
- **Process groups.** Engines are started in their own process group and
  signalled as a group, so wrapper scripts and child processes die with them.
- **No starting on an occupied port.** Before spawning, llm-pivot checks
  that nothing already listens on the engine's port. A stranded engine would
  otherwise answer the health probe and silently serve the wrong model.
- **No deadline on generation.** The router sets no write timeout and no
  response-header timeout, so an engine may take minutes to produce the first
  token of a cold model without being cut off.
- **Config is never mutated in place.** Each change is applied to a copy,
  validated, written to disk, and only then published — a rejected edit
  cannot leave a half-applied configuration behind.
- **The page polls once a second** on a single endpoint that carries status
  and new log lines together; forms are not re-rendered while you type.

## Development

```sh
go test ./...          # unit + end-to-end tests (spawns real child processes)
go test -race ./...
go vet ./...
```

The end-to-end tests re-execute the test binary as a stand-in LLM engine, so
they exercise real process spawning, health waiting, killing, model switching
through the router, and the configuration editing API.
