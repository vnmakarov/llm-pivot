# llm-pivot

A small Go server that starts, stops and monitors local LLM engines
(llama.cpp, strata, ninfer, …) and routes OpenAI-compatible traffic to the
engine serving the requested model.

Two ports:

| Port             | Default | What it is                                            |
|------------------|---------|-------------------------------------------------------|
| **monitor port** | `8061`  | Web page: configure engines, start/stop, watch output |
| **router port**  | `8078`  | OpenAI-compatible endpoint, forwarded to the engine   |

**One engine with one model runs at a time.** Starting a model kills whatever
was running before.

llm-pivot is mostly written by Claude Opus 5.

<a href="Monitor-Page.png">
  <img src="Monitor-Page.png" alt="Click to enlarge">
</a>


## Quick start

Requires Go 1.26.6 or newer and a Unix-like system with process-group signals.
From the repository directory:

```sh
go build -o llm-pivot .
./llm-pivot
```

Open <http://127.0.0.1:8061>, set the engine command, and add a model. First
start creates `~/.config/llm-pivot/config.yaml` by default. The web page is
compiled into the binary.

Point OpenAI-compatible clients at `http://127.0.0.1:8078/v1` and use the
configured model name. The router starts its engine automatically.

[Doc.md](Doc.md) contains more details: installation, systemd setup, the
monitor page, configuration, routing, command-line flags, the HTTP API,
development, and troubleshooting.
