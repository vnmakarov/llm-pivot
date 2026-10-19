package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
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
	mux.Handle("GET /", noCache(files))
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("GET /api/command", a.command)
	mux.HandleFunc("PUT /api/settings", a.settings)
	mux.HandleFunc("POST /api/engines", a.saveEngine)
	mux.HandleFunc("DELETE /api/engines/{engine}", a.deleteEngine)
	mux.HandleFunc("POST /api/models", a.saveModel)
	mux.HandleFunc("DELETE /api/engines/{engine}/models/{model}", a.deleteModel)
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

func (a *API) state(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	lines, seq, gap := a.mgr.Log().Since(since)
	cfg := a.mgr.Config()
	writeJSON(w, http.StatusOK, StateResponse{
		Config:     cfg,
		Status:     a.mgr.Status(),
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
