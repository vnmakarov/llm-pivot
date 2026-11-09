package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

// StateResponse is everything the monitor page needs for one refresh.
type StateResponse struct {
	Config     *Config   `json:"config"`
	Status     Status    `json:"status"`
	Log        []LogLine `json:"log"`
	LogSeq     uint64    `json:"log_seq"`
	LogGap     bool      `json:"log_gap"`
	ConfigPath string    `json:"config_path"`
	Version    string    `json:"version"`
	Now        string    `json:"now"`
}

// API serves the monitor page and its REST endpoints.
type API struct {
	mgr *Manager
	// rebind is called after the ports or the bind host changed.
	rebind func(host string, monitorPort, routerPort int)
}

func NewAPI(mgr *Manager, rebind func(string, int, int)) http.Handler {
	a := &API{mgr: mgr, rebind: rebind}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	// The page's own files answer GET and HEAD; anything else addressed to
	// them is a mistake, and saying so is better than handing back the page.
	// The pattern carries no method because "GET /" and "/engine/" overlap
	// without either being the more specific, which ServeMux refuses.
	mux.Handle("/", noCache(readOnly(files)))
	// The running engine's own monitoring page, fetched by llm-pivot and
	// served from here so that the browser never has to reach the engine.
	// Everything below the prefix is forwarded, so the page's relative
	// links, stylesheets and XHRs resolve against it and keep working.
	mux.Handle("/engine/", http.StripPrefix("/engine", http.HandlerFunc(a.engineFrame)))
	mux.HandleFunc("/engine", func(w http.ResponseWriter, r *http.Request) {
		// Without the trailing slash the page's relative links would
		// resolve against "/" and escape the proxy.
		http.Redirect(w, r, "/engine/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("GET /api/command", a.command)
	mux.HandleFunc("PUT /api/settings", a.settings)
	mux.HandleFunc("POST /api/engines", a.saveEngine)
	mux.HandleFunc("DELETE /api/engines/{engine}", a.deleteEngine)
	mux.HandleFunc("POST /api/engines/{engine}/move", a.moveEngine)
	mux.HandleFunc("POST /api/models", a.saveModel)
	mux.HandleFunc("DELETE /api/engines/{engine}/models/{model}", a.deleteModel)
	mux.HandleFunc("POST /api/engines/{engine}/models/{model}/move", a.moveModel)
	mux.HandleFunc("POST /api/control", a.control)
	mux.HandleFunc("POST /api/log/clear", a.clearLog)
	return mux
}

// noCache keeps browsers from holding on to a stale page after an upgrade.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// readOnly refuses anything but a read on the static page files, which the
// file server would otherwise answer whatever the method.
func readOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "only GET and HEAD are served here")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// writeFrameError answers inside the monitoring frame, where a browser
// renders whatever comes back as a document: JSON there is a wall of
// punctuation across an otherwise empty panel.
func writeFrameError(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<body style="margin:0;padding:24px;font:14px/1.6 system-ui,sans-serif;
             color:#c9d1d9;background:#0d1117">
<p style="margin:0 0 6px;font-weight:600">%s</p>
<p style="margin:0;color:#8b949e">%s</p>
</body>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(detail))
}

// engineFrame proxies the running engine's own monitoring page, so that the
// browser loads it from llm-pivot rather than having to reach the engine
// itself - which it generally cannot: the engine listens on llm-pivot's
// loopback, and that is a different machine's loopback in any browser not
// running on this one.
func (a *API) engineFrame(w http.ResponseWriter, r *http.Request) {
	target := a.mgr.EmbedTarget()
	if target == nil {
		st := a.mgr.Status()
		switch {
		case st.State == StateStarting:
			writeFrameError(w, http.StatusServiceUnavailable, "The engine is still starting.",
				"Its page will appear here as soon as it answers.")
		case st.State == StateFailed:
			writeFrameError(w, http.StatusServiceUnavailable, "The engine failed.",
				st.LastError)
		default:
			writeFrameError(w, http.StatusServiceUnavailable,
				"No engine with a framed monitoring page is running.",
				"Start one from the left pane.")
		}
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL joins the target's origin with the path left by
			// StripPrefix, and points the Host header at the engine.
			pr.SetURL(target)
		},
		FlushInterval: -1, // a monitoring page may well stream
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the browser went away
			}
			// Say who dialled what: the address is on the llm-pivot
			// machine, not on the one reading this, and that is the first
			// thing anyone looking at it needs to know.
			writeFrameError(w, http.StatusBadGateway,
				"The engine's monitoring page did not answer.",
				fmt.Sprintf("llm-pivot tried %s on its own machine and got: %v", target, err))
		},
	}
	proxy.ServeHTTP(w, r)
}

func (a *API) state(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	lines, seq, gap := a.mgr.Log().Since(since)
	cfg := a.mgr.Config()
	st := a.mgr.Status()
	// The sampled body of an http-monitored engine is up to 512 KB and is
	// only ever displayed on the engine tab, so shipping it with every
	// one-second refresh is a continuous download for nothing. The caller
	// asks for it when it is actually looking at it; everything else about
	// the sample - status, time, sample number - always travels.
	if st.Monitor != nil && r.URL.Query().Get("monitor_body") != "1" {
		light := *st.Monitor
		light.Body = ""
		st.Monitor = &light
	}
	writeJSON(w, http.StatusOK, StateResponse{
		Config:     cfg,
		Status:     st,
		Log:        lines,
		LogSeq:     seq,
		LogGap:     gap,
		ConfigPath: cfg.Path(),
		Version:    Version,
		Now:        time.Now().Format("15:04:05"),
	})
}

// command shows the argument vector a model would really be started with,
// after the model options have been layered over the engine options.
func (a *API) command(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("model")
	cfg := a.mgr.Config()
	eng, mdl := cfg.FindModel(name)
	if eng == nil {
		writeError(w, http.StatusNotFound, "unknown model "+strconv.Quote(name))
		return
	}
	argv, err := buildArgv(eng, mdl)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"model":   name,
		"engine":  eng.Name,
		"command": QuoteArgs(argv),
	})
}

func (a *API) settings(w http.ResponseWriter, r *http.Request) {
	var in SettingsInput
	if !decode(w, r, &in) {
		return
	}
	res, err := a.mgr.UpdateSettings(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if res.PortsMoved {
		cfg := a.mgr.Config()
		res.Notes = append(res.Notes, "Ports updated. Reopening the listeners…")
		// Rebind after this response has been delivered, otherwise we would
		// close the connection that is carrying it.
		go func() {
			time.Sleep(400 * time.Millisecond)
			a.rebind(cfg.Host, cfg.MonitorPort, cfg.RouterPort)
		}()
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) saveEngine(w http.ResponseWriter, r *http.Request) {
	var in EngineInput
	if !decode(w, r, &in) {
		return
	}
	in.Env = splitEnv(in.Env)
	res, err := a.mgr.SaveEngine(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) deleteEngine(w http.ResponseWriter, r *http.Request) {
	res, err := a.mgr.DeleteEngine(r.PathValue("engine"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) saveModel(w http.ResponseWriter, r *http.Request) {
	var in ModelInput
	if !decode(w, r, &in) {
		return
	}
	res, err := a.mgr.SaveModel(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) deleteModel(w http.ResponseWriter, r *http.Request) {
	res, err := a.mgr.DeleteModel(r.PathValue("engine"), r.PathValue("model"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) moveEngine(w http.ResponseWriter, r *http.Request) {
	var in MoveInput
	if !decode(w, r, &in) {
		return
	}
	res, err := a.mgr.MoveEngine(r.PathValue("engine"), in.Direction)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) moveModel(w http.ResponseWriter, r *http.Request) {
	var in MoveInput
	if !decode(w, r, &in) {
		return
	}
	res, err := a.mgr.MoveModel(r.PathValue("engine"), r.PathValue("model"), in.Direction)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// control starts, stops or restarts a model. Starting or restarting always
// kills whatever engine was running before.
func (a *API) control(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
		Model  string `json:"model"`
	}
	if !decode(w, r, &in) {
		return
	}
	var err error
	switch in.Action {
	case "start":
		err = a.mgr.Start(in.Model)
	case "restart":
		err = a.mgr.Restart(in.Model)
	case "stop":
		err = a.mgr.Stop()
	default:
		writeError(w, http.StatusBadRequest, "unknown action "+strconv.Quote(in.Action))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": a.mgr.Status()})
}

func (a *API) clearLog(w http.ResponseWriter, r *http.Request) {
	a.mgr.Log().Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// splitEnv turns the free-form textarea content into KEY=VALUE entries.
func splitEnv(in []string) []string {
	var out []string
	for _, chunk := range in {
		for _, line := range strings.Split(chunk, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}
