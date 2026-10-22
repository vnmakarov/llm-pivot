package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxInspectBody caps how much of a request body is buffered in order to find
// the "model" field. Larger bodies are forwarded without inspection.
const maxInspectBody = 64 << 20

// Response headers that say who actually answered a request.
//
//	X-Llm-Pivot           always set; you are talking to llm-pivot
//	X-Llm-Pivot-Upstream  only on proxied replies: "<engine>/<model>:<port>"
//
// So a response carrying the Upstream header came from the engine, and one
// without it was produced by llm-pivot itself. That distinction is the
// difference between "my client asked for a path this engine does not serve"
// and "llm-pivot could not route this".
const (
	HeaderRouter   = "X-Llm-Pivot"
	HeaderUpstream = "X-Llm-Pivot-Upstream"
)

type targetCtxKey struct{}

// routeTarget is the engine a single request was aimed at.
type routeTarget struct {
	engine string
	model  string
	port   int
}

func (t routeTarget) String() string {
	return fmt.Sprintf("%s/%s:%d", t.engine, t.model, t.port)
}

// Router serves the OpenAI-compatible port. Every request is forwarded to the
// engine that is currently running; a request naming a different model first
// stops that engine and starts the right one.
type Router struct {
	mgr      *Manager
	proxy    *httputil.ReverseProxy
	throttle *logThrottle
}

func NewRouter(mgr *Manager) *Router {
	rt := &Router{mgr: mgr, throttle: newLogThrottle()}
	rt.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t, _ := pr.In.Context().Value(targetCtxKey{}).(routeTarget)
			pr.SetURL(&url.URL{
				Scheme: "http",
				Host:   net.JoinHostPort(engineHost, strconv.Itoa(t.port)),
			})
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			t, _ := resp.Request.Context().Value(targetCtxKey{}).(routeTarget)
			resp.Header.Set(HeaderUpstream, t.String())
			// Make the engine's own rejections visible on the monitor page.
			// Without this a 404 from the engine looks identical to a 404
			// from llm-pivot to anyone reading the page.
			if resp.StatusCode >= 400 {
				rt.logUpstreamError(t, resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			}
			return nil
		},
		// -1 flushes immediately, which keeps SSE token streaming live.
		FlushInterval: -1,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			MaxIdleConns:          64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 0, // generation can take a long time
			ExpectContinueTimeout: time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client went away
			}
			t, _ := r.Context().Value(targetCtxKey{}).(routeTarget)
			log.Printf("router: proxying %s %s failed: %v", r.Method, r.URL.Path, err)
			rt.mgr.Log().System(fmt.Sprintf("✗ engine %q did not answer %s %s: %v",
				t.engine, r.Method, r.URL.Path, err))
			writeOpenAIError(w, http.StatusBadGateway,
				"the engine did not answer: "+err.Error(), "engine_unavailable")
		},
	}
	return rt
}

// logUpstreamError notes an error status the engine itself returned, with a
// hint for the mistake this almost always is: a path the engine does not
// serve, usually a base URL missing its "/v1".
func (rt *Router) logUpstreamError(t routeTarget, method, path string, code int) {
	if !rt.throttle.allow(fmt.Sprintf("%d %s %s", code, method, path), 10*time.Second) {
		return
	}
	msg := fmt.Sprintf("engine %q answered %d to %s %s (the engine rejected it, not llm-pivot)",
		t.engine, code, method, path)
	if code == http.StatusNotFound && !strings.HasPrefix(path, "/v1/") {
		msg += fmt.Sprintf("; %q has no %q — most OpenAI engines only serve /v1/…, "+
			"so check that your client's base URL ends in /v1", t.engine, path)
	}
	rt.mgr.Log().System(msg)
}

// logThrottle keeps a client retrying a bad path from flooding the
// monitoring buffer.
type logThrottle struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newLogThrottle() *logThrottle {
	return &logThrottle{seen: map[string]time.Time{}}
}

func (t *logThrottle) allow(key string, every time.Duration) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.seen[key]; ok && now.Sub(last) < every {
		return false
	}
	if len(t.seen) > 64 { // bounded: drop entries that cannot suppress anything
		for k, v := range t.seen {
			if now.Sub(v) >= every {
				delete(t.seen, k)
			}
		}
	}
	t.seen[key] = now
	return true
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Stamped on everything, including the replies below that never reach an
	// engine, so a client can always tell llm-pivot is in the path.
	w.Header().Set(HeaderRouter, Version)

	switch {
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		rt.listModels(w)
		return
	case r.URL.Path == "/router/status":
		writeJSON(w, http.StatusOK, rt.mgr.Status())
		return
	}

	wanted, err := requestedModel(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	var target routeTarget
	if wanted != "" {
		cfg := rt.mgr.Config()
		if eng, _ := cfg.FindModel(wanted); eng == nil {
			writeOpenAIError(w, http.StatusNotFound,
				"model "+strconv.Quote(wanted)+" is not configured in llm-pivot; "+
					"add it on the monitor page or GET /v1/models for the list",
				"model_not_found")
			return
		}
		// This kills the engine currently serving another model.
		if err := rt.mgr.EnsureModel(wanted); err != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"could not start model "+strconv.Quote(wanted)+": "+err.Error(),
				"engine_start_failed")
			return
		}
		target.engine, target.model, target.port, _ = rt.mgr.RunningTarget()
	} else {
		var ready bool
		target.engine, target.model, target.port, ready = rt.mgr.RunningTarget()
		if target.model == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"no engine is running and the request does not name a model; "+
					"start one on the monitor page or send a \"model\" field",
				"no_engine_running")
			return
		}
		if !ready {
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"engine for model "+strconv.Quote(target.model)+" is still starting",
				"engine_starting")
			return
		}
	}
	if target.port == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable,
			"the engine stopped before the request could be forwarded", "no_engine_running")
		return
	}

	defer rt.mgr.MarkUsed()()
	rt.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetCtxKey{}, target)))
}

// requestedModel finds the model a request asks for, looking at the JSON body
// and at a "model" query parameter. The body is restored for forwarding.
func requestedModel(r *http.Request) (string, error) {
	if m := strings.TrimSpace(r.URL.Query().Get("model")); m != "" {
		return m, nil
	}
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "", nil
	}
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		return "", nil // multipart uploads and friends go to the running engine
	}
	if r.ContentLength > maxInspectBody {
		return "", nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInspectBody))
	r.Body.Close()
	if err != nil {
		return "", err
	}
	// Put the body back so the proxy can forward it unchanged.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return "", nil
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", nil // not our business; let the engine reject it
	}
	return strings.TrimSpace(probe.Model), nil
}

// listModels answers /v1/models from the configuration, so clients can see
// every model the router can route to, not just the running one.
func (rt *Router) listModels(w http.ResponseWriter) {
	cfg := rt.mgr.Config()
	_, running, _, _ := rt.mgr.RunningTarget()

	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		Running bool   `json:"running"`
	}
	out := struct {
		Object string     `json:"object"`
		Data   []modelObj `json:"data"`
	}{Object: "list", Data: []modelObj{}}

	now := time.Now().Unix()
	for _, e := range cfg.Engines {
		for _, m := range e.Models {
			out.Data = append(out.Data, modelObj{
				ID:      m.Name,
				Object:  "model",
				Created: now,
				OwnedBy: e.Name,
				Running: m.Name == running,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// writeOpenAIError replies in the shape OpenAI clients expect.
func writeOpenAIError(w http.ResponseWriter, code int, msg, kind string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    kind,
			"param":   nil,
			"code":    kind,
		},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
