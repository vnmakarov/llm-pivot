package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestHelperEngine is not a test: when ER_FAKE_ENGINE is set the test binary
// re-executes itself as a stand-in LLM engine. It understands --port and
// --tag, answers /health and /v1/chat/completions, and reports the tag so the
// test can tell which "model" is live.
func TestHelperEngine(t *testing.T) {
	if os.Getenv("ER_FAKE_ENGINE") != "1" {
		t.Skip("helper process")
	}
	var port, tag string
	args := os.Args
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
			}
		case "--tag":
			if i+1 < len(args) {
				tag = args[i+1]
			}
		}
	}
	// Some output on both streams, so stdio monitoring has something to show.
	fmt.Fprintf(os.Stdout, "fake engine stdout: serving %s\n", tag)
	fmt.Fprintf(os.Stderr, "fake engine stderr: listening on port %s\n", port)

	// ER_FAKE_HUGE_LINE prints one line far longer than the log buffer will
	// store, the way \r progress output accumulates with no newline in it.
	// The engine goes on writing afterwards: llm-pivot must still be reading,
	// or these writes die of SIGPIPE.
	if n, _ := strconv.Atoi(os.Getenv("ER_FAKE_HUGE_LINE")); n > 0 {
		fmt.Fprint(os.Stdout, strings.Repeat("progress\r", n))
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "fake engine stdout: still alive after the huge line")
	}

	delay, _ := strconv.Atoi(os.Getenv("ER_FAKE_DELAY_MS"))
	time.Sleep(time.Duration(delay) * time.Millisecond)

	if os.Getenv("ER_FAKE_CRASH") == "1" {
		fmt.Fprintln(os.Stderr, "fake engine stderr: refusing to start")
		os.Exit(3)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok"}`)
	})
	// A framed monitoring page, written the way real ones are: every link
	// relative, so it only works if whatever serves it keeps the prefix.
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><script src="assets/app.js"></script>%s</html>`, tag)
	})
	mux.HandleFunc("/ui/assets/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, "// the framed page's own script for %s\n", tag)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "engine_tag{tag=%q} 1\n", tag)
	})
	// Stands in for a slow generation: the reply only starts after ?ms.
	mux.HandleFunc("/v1/hang", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		io.WriteString(w, `{"slow":true}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("ER_FAKE_ANTHROPIC_BACKEND") == "1" {
			anthropicTestBackend(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Received-Host", r.Host)
		w.Header().Set("X-Received-Forwarded-Host", r.Header.Get("X-Forwarded-Host"))
		fmt.Fprintf(w, `{"served_by":%q,"echo":%s}`, tag, string(body))
	})
	// llama.cpp's native prompt renderer and tokenizer, which count_tokens
	// prefers. Without them an engine must still answer, by estimate.
	mux.HandleFunc("/apply-template", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if os.Getenv("ER_FAKE_TOKENIZER") != "1" || json.NewDecoder(r.Body).Decode(&in) != nil {
			http.NotFound(w, r)
			return
		}
		prompt := ""
		for _, m := range in.Messages {
			prompt += fmt.Sprint(m.Content) + "\n"
		}
		writeJSON(w, 200, map[string]string{"prompt": prompt})
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Content string `json:"content"`
		}
		if os.Getenv("ER_FAKE_TOKENIZER") != "1" || json.NewDecoder(r.Body).Decode(&in) != nil {
			http.NotFound(w, r)
			return
		}
		tokens := []int{}
		for i := range strings.Fields(in.Content) {
			tokens = append(tokens, i+1)
		}
		writeJSON(w, 200, map[string][]int{"tokens": tokens})
	})
	// Counts the body instead of echoing it, so a request far too big to
	// hold twice can still be checked for having arrived whole.
	mux.HandleFunc("/v1/size", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"served_by":%q,"bytes":%d}`, tag, n)
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}

	// ER_FAKE_EXIT_MS stands in for an engine that starts fine and then dies
	// on its own: /health answers first, so llm-pivot must learn about it from
	// the reaper rather than from a failed start.
	if died, _ := strconv.Atoi(os.Getenv("ER_FAKE_EXIT_MS")); died > 0 {
		go func() {
			time.Sleep(time.Duration(died) * time.Millisecond)
			srv.Close()
			os.Exit(7)
		}()
	}

	// Exit cleanly on the SIGTERM llm-pivot sends.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-stop; srv.Close(); os.Exit(0) }()

	srv.ListenAndServe()
	os.Exit(0)
}

// fakeEngineCommand builds the command line that re-executes the test binary
// as the fake engine. The bare "--" stops the testing flag parser, so the
// engine arguments after it are left alone.
func fakeEngineCommand() string {
	return QuoteArgs([]string{os.Args[0], "-test.run=^TestHelperEngine$", "--", "--tag", "{{model}}"})
}

var (
	issuedPortsMu sync.Mutex
	issuedPorts   = map[int]bool{}
)

// freePort picks a port the fake engine can bind. Asking the kernel for one
// and then closing the listener is inherently racy, so never hand out the
// same port twice within this process and re-check that it is still free.
func freePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()

		issuedPortsMu.Lock()
		taken := issuedPorts[p]
		if !taken {
			issuedPorts[p] = true
		}
		issuedPortsMu.Unlock()
		if taken {
			continue
		}
		// Nothing must have grabbed it in the meantime.
		if c, err := net.DialTimeout("tcp", l.Addr().String(), 150*time.Millisecond); err == nil {
			c.Close()
			continue
		}
		return p
	}
	t.Fatal("could not find a free port")
	return 0
}

// testManager wires up a manager whose single engine is the fake one.
func testManager(t *testing.T, models []*Model, tweak func(*Engine)) *Manager {
	t.Helper()
	eng := &Engine{
		Name:       "fake",
		Port:       freePort(t),
		Command:    fakeEngineCommand(),
		Monitor:    MonitorStdio,
		HealthPath: "/health",
		PortFlag:   "--port",
		Env:        []string{"ER_FAKE_ENGINE=1"},
		Models:     models,
	}
	if tweak != nil {
		tweak(eng)
	}
	cfg := &Config{
		MonitorPort: freePort(t),
		RouterPort:  freePort(t),
		Engines:     []*Engine{eng},
		path:        filepath.Join(t.TempDir(), "config.yaml"),
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	return mgr
}

func TestManagerStartStopRestart(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha", Options: ""}}, nil)

	if err := mgr.Start("alpha"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := mgr.Status()
	if st.State != StateRunning || st.Model != "alpha" || st.PID == 0 {
		t.Fatalf("unexpected status %+v", st)
	}
	firstPID := st.PID

	// The engine's own output must have reached the monitoring buffer.
	lines, _, _ := mgr.Log().Since(0)
	var all strings.Builder
	for _, l := range lines {
		all.WriteString(l.Stream + ": " + l.Text + "\n")
	}
	for _, want := range []string{"stdout: fake engine stdout", "stderr: fake engine stderr"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("monitoring buffer is missing %q; got:\n%s", want, all.String())
		}
	}

	if err := mgr.Restart("alpha"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if pid := mgr.Status().PID; pid == firstPID || pid == 0 {
		t.Errorf("restart kept pid %d", pid)
	}
	assertProcessGone(t, firstPID)

	if err := mgr.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := mgr.Status(); st.State != StateIdle || st.Model != "" {
		t.Fatalf("after Stop: %+v", st)
	}
}

func TestManagerOnlyOneEngineAtATime(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}, {Name: "beta"}}, nil)

	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	alphaPID := mgr.Status().PID

	if err := mgr.Start("beta"); err != nil {
		t.Fatal(err)
	}
	st := mgr.Status()
	if st.Model != "beta" {
		t.Fatalf("running model is %q, want beta", st.Model)
	}
	if st.PID == alphaPID {
		t.Fatal("beta reused alpha's process")
	}
	assertProcessGone(t, alphaPID)
}

func TestManagerReportsStartupFailure(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Env = append(e.Env, "ER_FAKE_CRASH=1")
		e.StartupTimeout = 10
	})
	err := mgr.Start("alpha")
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("unhelpful error: %v", err)
	}
	st := mgr.Status()
	if st.State != StateFailed || st.LastError == "" {
		t.Errorf("status after failure: %+v", st)
	}
	// The engine never bound its port, so no instance exists to name it: only
	// failed_engine/failed_model can tell the page which model to light red.
	if st.Model != "" || st.FailedModel != "alpha" || st.FailedEngine != "fake" {
		t.Errorf("failure not attributed to alpha: %+v", st)
	}
}

// The engine that dies after coming up leaves no instance behind, and the
// failure must still be pinned to the model it was serving.
func TestDeathWhileRunningNamedTheModel(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Env = append(e.Env, "ER_FAKE_EXIT_MS=1500")
	})
	if err := mgr.Start("alpha"); err != nil {
		t.Fatalf("Start: %v", err) // the engine answers /health before it dies
	}

	deadline := time.Now().Add(15 * time.Second)
	var st Status
	for {
		st = mgr.Status()
		if st.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the death was never reported: %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.Model != "" {
		// The process is gone, so Status.model must go back to nothing.
		t.Errorf("a dead engine must not be reported as running: %+v", st)
	}
	if st.FailedModel != "alpha" || st.FailedEngine != "fake" {
		t.Errorf("death not attributed to alpha: %+v", st)
	}
	if !strings.Contains(st.LastError, "died") {
		t.Errorf("unhelpful error: %q", st.LastError)
	}
}

// A stranded engine from an earlier manual run must not be mistaken for a
// successful start: its health endpoint would answer, and requests would
// then be served by the wrong model.
func TestStartRefusesAnOccupiedPort(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	port := mgr.Config().Engines[0].Port

	squatter, err := net.Listen("tcp", net.JoinHostPort(engineHost, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("could not occupy port %d: %v", port, err)
	}
	defer squatter.Close()

	err = mgr.Start("alpha")
	if err == nil {
		t.Fatal("Start must refuse a port that is already in use")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("unhelpful error: %v", err)
	}
	st := mgr.Status()
	if st.State != StateFailed {
		t.Errorf("state = %q, want failed", st.State)
	}
	if st.PID != 0 {
		t.Errorf("no engine process should have been spawned, got pid %d", st.PID)
	}

	// Once the squatter goes away the same model starts normally.
	squatter.Close()
	if err := mgr.Start("alpha"); err != nil {
		t.Fatalf("Start after the port was freed: %v", err)
	}
}

func TestWaitPortFree(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port

	if err := waitPortFree(port, 300*time.Millisecond); err == nil {
		t.Error("an occupied port must be reported")
	}
	l.Close()
	if err := waitPortFree(port, time.Second); err != nil {
		t.Errorf("a free port must be accepted: %v", err)
	}
}

// Regression: probing the port with a connect used to report "free" here.
// A listener with a one-deep backlog that never accepts lets the first
// connect through and makes every later one time out, which looks exactly
// like an unused port. Binding is immune to that.
func TestWaitPortFreeWithAFullBacklog(t *testing.T) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	sa := &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil { // smallest possible backlog
		t.Fatal(err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := name.(*syscall.SockaddrInet4).Port

	// Fill the accept queue; nothing ever calls accept on this socket.
	for i := 0; i < 3; i++ {
		if c, err := net.DialTimeout("tcp",
			net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond); err == nil {
			defer c.Close()
		}
	}

	if err := waitPortFree(port, 300*time.Millisecond); err == nil {
		t.Fatal("a listening socket with a full backlog must still count as occupied")
	}
}

func TestManagerUnknownModel(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	if err := mgr.Start("nope"); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPMonitoring(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Monitor = MonitorHTTP
		e.MonitorURL = "http://127.0.0.1:{{port}}/metrics"
		e.MonitorEvery = 1
	})
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if snap := mgr.Status().Monitor; snap != nil && strings.Contains(snap.Body, "engine_tag") {
			if snap.Status != 200 {
				t.Errorf("snapshot status = %d", snap.Status)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no http monitoring sample arrived; status %+v", mgr.Status())
}

// ------------------------------------------------------------------ router

func TestRouterSwitchesModelOnRequest(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}, {Name: "beta"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	ask := func(model string) map[string]any {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("bad JSON %q: %v", raw, err)
		}
		return out
	}

	// Nothing is running: the first request must start the right engine.
	if got := ask("alpha")["served_by"]; got != "alpha" {
		t.Fatalf("served_by = %v, want alpha", got)
	}
	alphaPID := mgr.Status().PID

	// A second request for the same model must reuse the engine.
	if got := ask("alpha")["served_by"]; got != "alpha" {
		t.Fatalf("served_by = %v, want alpha", got)
	}
	if mgr.Status().PID != alphaPID {
		t.Error("the engine was restarted for the same model")
	}

	// A request for another model kills the engine and starts the new one.
	if got := ask("beta")["served_by"]; got != "beta" {
		t.Fatalf("served_by = %v, want beta", got)
	}
	if mgr.Status().Model != "beta" {
		t.Fatalf("running model = %q", mgr.Status().Model)
	}
	assertProcessGone(t, alphaPID)

	// The body must survive the detour unchanged.
	echo := ask("beta")["echo"].(map[string]any)
	if echo["model"] != "beta" {
		t.Errorf("forwarded body lost the model field: %v", echo)
	}
}

func TestRouterUnknownModelIs404(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"ghost"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var out struct {
		Error struct{ Message, Code string }
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Error.Code != "model_not_found" || !strings.Contains(out.Error.Message, "ghost") {
		t.Errorf("unhelpful error payload: %+v", out.Error)
	}
	if mgr.Status().State != StateIdle {
		t.Error("an unknown model must not disturb the running engine")
	}
}

func TestRouterNoEngineRunning(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestRouterListsEveryConfiguredModel(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}, {Name: "beta"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object string
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("unexpected payload %+v", out)
	}
	if out.Data[0].ID != "alpha" || out.Data[1].ID != "beta" {
		t.Errorf("ids = %v", out.Data)
	}
}

func TestRouterForwardsUnrelatedPathsToRunningEngine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"status":"ok"`)) {
		t.Fatalf("GET /health returned %q", body)
	}
}

func TestRouterHostHeaderPointsAtEngine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alpha","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "192.168.0.110:8078"
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	wantHost := fmt.Sprintf("%s:%d", engineHost, mgr.Config().Engines[0].Port)
	if got := resp.Header.Get("X-Received-Host"); got != wantHost {
		t.Errorf("engine received Host %q, want %q", got, wantHost)
	}
	if got := resp.Header.Get("X-Received-Forwarded-Host"); got != "192.168.0.110:8078" {
		t.Errorf("engine received X-Forwarded-Host %q, want 192.168.0.110:8078", got)
	}
}

// logText returns the whole monitoring buffer as one string.
func logText(mgr *Manager) string {
	lines, _, _ := mgr.Log().Since(0)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestUpstreamErrorsAreAttributed covers the confusing case: the engine
// itself answers 404 because the client asked for a path it does not serve.
// The reply must be marked as coming from the engine, and the monitor page
// must say so.
func TestUpstreamErrorsAreAttributed(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	// "/chat/completions" (no /v1) is not served by the fake engine, exactly
	// like a client whose base URL is missing its /v1.
	resp, err := http.Post(srv.URL+"/chat/completions", "application/json",
		strings.NewReader(`{"model":"alpha"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from the engine", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderUpstream); got != "fake/alpha:"+strconv.Itoa(mgr.Status().Port) {
		t.Errorf("%s = %q, want the engine that answered", HeaderUpstream, got)
	}
	if resp.Header.Get(HeaderRouter) != Version {
		t.Errorf("%s = %q, want %q", HeaderRouter, resp.Header.Get(HeaderRouter), Version)
	}

	text := logText(mgr)
	for _, want := range []string{
		`engine "fake" answered 404 to POST /chat/completions`,
		"not llm-pivot",
		"base URL ends in /v1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the monitoring buffer should mention %q; got:\n%s", want, text)
		}
	}
}

// TestRouterOwnErrorsCarryNoUpstreamHeader is the other half: a reply that
// llm-pivot produced itself must not look like it came from an engine.
func TestRouterOwnErrorsCarryNoUpstreamHeader(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"ghost"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderUpstream); got != "" {
		t.Errorf("%s = %q, want it absent on llm-pivot's own reply", HeaderUpstream, got)
	}
	if resp.Header.Get(HeaderRouter) != Version {
		t.Errorf("%s is missing", HeaderRouter)
	}
}

func TestSuccessfulProxyIsAttributedToo(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"alpha"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderUpstream); !strings.HasPrefix(got, "fake/alpha:") {
		t.Errorf("%s = %q", HeaderUpstream, got)
	}
}

// A client retrying a bad path must not flood the monitoring buffer.
func TestUpstreamErrorLoggingIsThrottled(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	for i := 0; i < 12; i++ {
		resp, err := http.Post(srv.URL+"/chat/completions", "application/json",
			strings.NewReader(`{"model":"alpha"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	n := strings.Count(logText(mgr), "answered 404 to POST /chat/completions")
	if n != 1 {
		t.Errorf("logged the same engine 404 %d times, want 1", n)
	}
}

func TestLogThrottle(t *testing.T) {
	th := newLogThrottle()
	if !th.allow("k", time.Minute) {
		t.Fatal("the first message must pass")
	}
	if th.allow("k", time.Minute) {
		t.Error("the immediate repeat must be suppressed")
	}
	if !th.allow("other", time.Minute) {
		t.Error("a different key must pass")
	}
	if !th.allow("k", 0) {
		t.Error("the message must pass again once the window elapsed")
	}
}

// --------------------------------------------------------------- edit API

func TestEditingARunningEngineStopsIt(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	pid := mgr.Status().PID

	res, err := mgr.SaveModel(ModelInput{
		Engine: "fake", Original: "alpha", Name: "alpha", Options: "--extra 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) == 0 {
		t.Error("the user should be told that the engine was stopped")
	}
	if st := mgr.Status(); st.State != StateIdle {
		t.Errorf("engine still %s after its model changed", st.State)
	}
	assertProcessGone(t, pid)
}

func TestEditRejectsDuplicateModelName(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}, {Name: "beta"}}, nil)
	_, err := mgr.SaveModel(ModelInput{Engine: "fake", Name: "alpha"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v", err)
	}
	// The rejected change must not have been written anywhere.
	if n := len(mgr.Config().Engines[0].Models); n != 2 {
		t.Fatalf("model count = %d, want 2", n)
	}
}

// A form the server rejects leaves the configuration file alone. The test
// manager's file does not exist yet, so anything the rejected save wrote -
// an engine with its blank fields, say - would show up as a new file.
func TestRejectedNewEngineIsNotSaved(t *testing.T) {
	mgr := testManager(t, nil, nil)
	// No command: Validate rejects it, the way the page's empty field would.
	_, err := mgr.SaveEngine(EngineInput{Name: "broken", Port: freePort(t)})
	if err == nil {
		t.Fatal("an engine without a command must be rejected")
	}
	if _, statErr := os.Stat(mgr.Config().Path()); !os.IsNotExist(statErr) {
		t.Fatalf("the rejected engine reached the file: %v", statErr)
	}
	if n := len(mgr.Config().Engines); n != 1 {
		t.Fatalf("engine count = %d, want the original one only", n)
	}
}

func TestEditPersistsToDisk(t *testing.T) {
	mgr := testManager(t, nil, nil)
	if _, err := mgr.SaveModel(ModelInput{
		Engine: "fake", Name: "gamma", Options: "-m /g.gguf",
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(mgr.Config().Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, m := reloaded.FindModel("gamma"); m == nil || m.Options != "-m /g.gguf" {
		t.Fatalf("the model was not persisted: %+v", reloaded.Engines[0].Models)
	}
}

func TestDeleteEngineRemovesItsModels(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	if _, err := mgr.DeleteEngine("fake"); err != nil {
		t.Fatal(err)
	}
	cfg := mgr.Config()
	if len(cfg.Engines) != 0 {
		t.Fatalf("engines left: %+v", cfg.Engines)
	}
	if e, _ := cfg.FindModel("alpha"); e != nil {
		t.Error("the model outlived its engine")
	}
}

// ------------------------------------------------------- reordering

// orderingManager adds a second model to the first engine, so that both the
// engine list and a model list have an inside and two ends.
func orderingManager(t *testing.T) *Manager {
	t.Helper()
	mgr := twoEngineManager(t)
	if _, err := mgr.SaveModel(ModelInput{Engine: "eng-alpha", Name: "alpha2"}); err != nil {
		t.Fatal(err)
	}
	return mgr
}

func engineOrder(c *Config) []string {
	names := []string{}
	for _, e := range c.Engines {
		names = append(names, e.Name)
	}
	return names
}

func modelOrder(c *Config, engine string) []string {
	names := []string{}
	for _, m := range c.Engine(engine).Models {
		names = append(names, m.Name)
	}
	return names
}

func TestMoveReordersAndPersists(t *testing.T) {
	mgr := orderingManager(t)
	if _, err := mgr.MoveEngine("eng-beta", "up"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.MoveModel("eng-alpha", "alpha", "down"); err != nil {
		t.Fatal(err)
	}
	// The order must survive the write, not just the in-memory swap.
	reloaded, err := LoadConfig(mgr.Config().Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Config{mgr.Config(), reloaded} {
		if got := engineOrder(c); !reflect.DeepEqual(got, []string{"eng-beta", "eng-alpha"}) {
			t.Errorf("engines: %v", got)
		}
		if got := modelOrder(c, "eng-alpha"); !reflect.DeepEqual(got, []string{"alpha2", "alpha"}) {
			t.Errorf("models: %v", got)
		}
	}
}

func TestMoveRejectsTheEndsAndUnknownNames(t *testing.T) {
	mgr := orderingManager(t)
	for _, tc := range []struct {
		what, detail string
		move         func() (*EditResult, error)
	}{
		{"engine first", "already first", func() (*EditResult, error) { return mgr.MoveEngine("eng-alpha", "up") }},
		{"engine last", "already last", func() (*EditResult, error) { return mgr.MoveEngine("eng-beta", "down") }},
		{"bad direction", "unknown direction", func() (*EditResult, error) { return mgr.MoveEngine("eng-alpha", "sideways") }},
		{"unknown engine", "unknown engine", func() (*EditResult, error) { return mgr.MoveEngine("nothing", "up") }},
		{"model first", "already first", func() (*EditResult, error) { return mgr.MoveModel("eng-alpha", "alpha", "up") }},
		{"model last", "already last", func() (*EditResult, error) { return mgr.MoveModel("eng-alpha", "alpha2", "down") }},
		{"only model", "already first", func() (*EditResult, error) { return mgr.MoveModel("eng-beta", "beta", "up") }},
		{"unknown model", "has no model", func() (*EditResult, error) { return mgr.MoveModel("eng-alpha", "nothing", "up") }},
	} {
		if _, err := tc.move(); err == nil || !strings.Contains(err.Error(), tc.detail) {
			t.Errorf("%s: %v", tc.what, err)
		}
	}
	// A rejected move must leave the configuration exactly as it was.
	cfg := mgr.Config()
	if got := engineOrder(cfg); !reflect.DeepEqual(got, []string{"eng-alpha", "eng-beta"}) {
		t.Errorf("engines disturbed: %v", got)
	}
	if got := modelOrder(cfg, "eng-alpha"); !reflect.DeepEqual(got, []string{"alpha", "alpha2"}) {
		t.Errorf("models disturbed: %v", got)
	}
}

// Reordering is presentational, so unlike every other edit it must not
// interrupt whatever is running.
func TestMoveLeavesTheRunningEngineAlone(t *testing.T) {
	mgr := orderingManager(t)
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	pid := mgr.Status().PID
	res, err := mgr.MoveModel("eng-alpha", "alpha", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes: %v", res.Notes)
	}
	if _, err := mgr.MoveEngine("eng-alpha", "down"); err != nil {
		t.Fatal(err)
	}
	if st := mgr.Status(); st.Model != "alpha" || st.PID != pid {
		t.Fatalf("the move restarted or stopped the engine: %+v", st)
	}
}

func TestMoveOverHTTP(t *testing.T) {
	mgr := orderingManager(t)
	srv := httptest.NewServer(NewAPI(mgr, func(string, int, int) {}))
	defer srv.Close()

	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/api/engines/eng-beta/move", `{"direction":"up"}`); code != 200 {
		t.Errorf("move engine: %d", code)
	}
	if code := post("/api/engines/eng-alpha/models/alpha2/move", `{"direction":"up"}`); code != 200 {
		t.Errorf("move model: %d", code)
	}
	if code := post("/api/engines/eng-alpha/models/alpha2/move", `{"direction":"up"}`); code != 400 {
		t.Errorf("move past the end: %d", code)
	}
	cfg := mgr.Config()
	if got := engineOrder(cfg); !reflect.DeepEqual(got, []string{"eng-beta", "eng-alpha"}) {
		t.Errorf("engines: %v", got)
	}
	if got := modelOrder(cfg, "eng-alpha"); !reflect.DeepEqual(got, []string{"alpha2", "alpha"}) {
		t.Errorf("models: %v", got)
	}
}

// ------------------------------------------------------- inactive stop

// ageLastUsed backdates the running instance, so the idle reaper can be tested
// without sleeping through a real timeout.
func ageLastUsed(t *testing.T, mgr *Manager, d time.Duration) {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.inst == nil {
		t.Fatal("nothing is running")
	}
	mgr.inst.lastUsed = time.Now().Add(-d)
}

func TestInactiveStopTimeoutStopsAnIdleEngine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	mgr.cfg.InactiveStopTimeout = 60

	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	pid := mgr.Status().PID

	if mgr.CheckInactivity() {
		t.Fatal("a freshly started engine was reaped as idle")
	}
	if st := mgr.Status(); st.State != StateRunning {
		t.Fatalf("engine should still run: %+v", st)
	}

	ageLastUsed(t, mgr, 61*time.Second)
	if !mgr.CheckInactivity() {
		t.Fatal("an engine idle past the timeout was not stopped")
	}
	if st := mgr.Status(); st.State != StateIdle || st.Model != "" {
		t.Fatalf("after the idle stop: %+v", st)
	}
	assertProcessGone(t, pid)

	// Nothing is running any more, so there is nothing left to reap.
	if mgr.CheckInactivity() {
		t.Error("CheckInactivity reported a stop with no engine running")
	}
}

func TestInactiveStopTimeoutNegativeNeverStops(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	mgr.cfg.InactiveStopTimeout = -1

	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	ageLastUsed(t, mgr, 365*24*time.Hour)
	if mgr.CheckInactivity() {
		t.Fatal("a negative inactive_stop_timeout_seconds must disable the idle stop")
	}
	if st := mgr.Status(); st.State != StateRunning {
		t.Fatalf("engine should still run: %+v", st)
	}
}

func TestRoutedRequestKeepsEngineAlive(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	mgr.cfg.InactiveStopTimeout = 60
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	ageLastUsed(t, mgr, 61*time.Second)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"alpha","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The request reset the idle clock, so the engine must survive the sweep.
	if mgr.CheckInactivity() {
		t.Fatal("an engine that just served a request was reaped as idle")
	}
	if st := mgr.Status(); st.State != StateRunning {
		t.Fatalf("engine should still run: %+v", st)
	}
}

func TestInFlightRequestIsNeverReapedAsIdle(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	mgr.cfg.InactiveStopTimeout = 60
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	pid := mgr.Status().PID

	// A generation that outlives the idle timeout by itself.
	done := make(chan int, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/v1/hang?model=alpha&ms=1500")
		if err != nil {
			done <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()

	// Wait until the proxy really has the request in flight.
	for deadline := time.Now().Add(5 * time.Second); ; {
		mgr.mu.RLock()
		n := mgr.inflight
		mgr.mu.RUnlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the proxy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ageLastUsed(t, mgr, time.Hour)
	if mgr.CheckInactivity() {
		t.Fatal("an engine with a request in flight was reaped as idle")
	}

	if code := <-done; code != 200 {
		t.Fatalf("the slow request did not survive: status %d", code)
	}
	assertProcessAlive(t, pid)

	// Finishing the request refreshed the idle clock.
	if mgr.CheckInactivity() {
		t.Fatal("the completed request did not reset the idle clock")
	}
}

func assertProcessAlive(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("process %d is gone: %v", pid, err)
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("process %d is still alive", pid)
}

// overrideManager puts command-line overrides on the manager's live
// configuration, as main does before handing it over, and writes the file
// the way it would be on disk - without them.
func overrideManager(t *testing.T, mgr *Manager, monitorPort, routerPort int, host string) {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.cfg.Overrides(monitorPort, routerPort, host)
	if err := mgr.cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// A reload re-reads the file, which knows nothing of the command line. The
// overrides this run was started with have to go back on, or systemctl
// reload silently un-applies them - and PortsMoved then drags the listeners
// back to the file's ports.
func TestReloadKeepsTheCommandLineOverrides(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	filePort := mgr.Config().MonitorPort
	flagPort := freePort(t)
	overrideManager(t, mgr, flagPort, 0, "")

	res, err := mgr.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.Config().MonitorPort; got != flagPort {
		t.Errorf("monitor_port after the reload = %d, want the override %d", got, flagPort)
	}
	if res.PortsMoved {
		t.Error("the reload asked to move the listeners although nothing moved")
	}
	if res.MonitorPort != flagPort {
		t.Errorf("the reload reported port %d, want %d", res.MonitorPort, flagPort)
	}
	// The file itself must still carry its own port, untouched by all this.
	onDisk, err := LoadConfig(mgr.Config().Path())
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.MonitorPort != filePort {
		t.Errorf("monitor_port in the file = %d, want %d", onDisk.MonitorPort, filePort)
	}
}

// The page saves its settings form over and over; none of those saves may
// bake the override into the file, and the live port must not budge.
func TestSettingsSaveDoesNotBakeInTheOverride(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	filePort, fileRouter := mgr.Config().MonitorPort, mgr.Config().RouterPort
	flagPort := freePort(t)
	overrideManager(t, mgr, flagPort, 0, "")

	// What the page submits: the values it was shown, with one unrelated
	// field changed.
	shown := mgr.Config()
	res, err := mgr.UpdateSettings(SettingsInput{
		MonitorPort: shown.MonitorPort,
		RouterPort:  shown.RouterPort,
		Host:        shown.Host,
		LogLines:    256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PortsMoved {
		t.Error("an unrelated settings save asked to move the listeners")
	}
	if got := mgr.Config().MonitorPort; got != flagPort {
		t.Errorf("the override stopped applying after a save: %d, want %d", got, flagPort)
	}
	onDisk, err := LoadConfig(mgr.Config().Path())
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.MonitorPort != filePort || onDisk.RouterPort != fileRouter {
		t.Errorf("the save baked the override into the file: %d %d, want %d %d",
			onDisk.MonitorPort, onDisk.RouterPort, filePort, fileRouter)
	}
	if onDisk.LogLines != 256 {
		t.Errorf("the save lost the real edit: log_lines = %d", onDisk.LogLines)
	}
}

// ------------------------------------------- oversized request bodies

// jsonBody builds a JSON request body of at least n bytes which names a
// model, standing in for a chat request carrying inline base64 audio.
func jsonBody(model string, n int) []byte {
	head := fmt.Sprintf(`{"model":%q,"blob":"`, model)
	tail := `"}`
	pad := n - len(head) - len(tail)
	if pad < 0 {
		pad = 0
	}
	return []byte(head + strings.Repeat("x", pad) + tail)
}

// A chunked request carries ContentLength -1, so the size pre-check never
// catches it. requestedModel must then not rebuild the body out of the
// prefix it buffered: that forwards the request truncated at the cap, with
// a ContentLength that makes the truncation look deliberate.
func TestOversizedChunkedBodyIsForwardedWhole(t *testing.T) {
	const limit = 64
	full := jsonBody("alpha", 4*limit)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(full))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = -1 // what a chunked request looks like
	r.TransferEncoding = []string{"chunked"}

	model, err := requestedModel(r, limit)
	if err != nil {
		t.Fatal(err)
	}
	if model != "" {
		t.Errorf("a body over the cap must not be sniffed, got model %q", model)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("the body was forwarded truncated: %d of %d bytes", len(got), len(full))
	}
	// Rewriting the framing is what made the truncation silent.
	if r.ContentLength != -1 || len(r.TransferEncoding) != 1 {
		t.Errorf("the framing was rewritten: ContentLength %d, TransferEncoding %v",
			r.ContentLength, r.TransferEncoding)
	}
}

// The boundary the other way: a body that exactly fills the cap is not over
// it, so it must still be read, sniffed and put back.
func TestBodyExactlyAtTheCapIsStillSniffed(t *testing.T) {
	full := jsonBody("alpha", 64)
	limit := int64(len(full))

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(full))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}

	model, err := requestedModel(r, limit)
	if err != nil {
		t.Fatal(err)
	}
	if model != "alpha" {
		t.Errorf("model = %q, want alpha", model)
	}
	got, _ := io.ReadAll(r.Body)
	if !bytes.Equal(got, full) {
		t.Errorf("the restored body differs: %d of %d bytes", len(got), len(full))
	}
	if r.ContentLength != limit {
		t.Errorf("ContentLength = %d, want %d", r.ContentLength, limit)
	}
	// A buffered body can be replayed, which is what GetBody promises.
	if r.GetBody == nil {
		t.Fatal("GetBody was not set for a buffered body")
	}
	rc, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := io.ReadAll(rc); !bytes.Equal(again, full) {
		t.Error("GetBody did not replay the body")
	}
}

// A declared length over the cap is refused inspection without the body
// being touched at all - the behaviour the chunked case now matches.
func TestDeclaredOversizedBodyIsLeftAlone(t *testing.T) {
	full := jsonBody("alpha", 256)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(full))
	r.Header.Set("Content-Type", "application/json")

	model, err := requestedModel(r, 64)
	if err != nil {
		t.Fatal(err)
	}
	if model != "" {
		t.Errorf("model = %q, want none", model)
	}
	if got, _ := io.ReadAll(r.Body); !bytes.Equal(got, full) {
		t.Errorf("the body was consumed: %d of %d bytes", len(got), len(full))
	}
}

// The same thing through the real router at the real cap: a chunked request
// bigger than maxInspectBody must reach the engine byte for byte.
func TestRouterForwardsAHugeChunkedBodyWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates more than the inspection cap")
	}
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	// One byte past the cap is enough to prove nothing was cut off, and
	// streaming it keeps a second copy out of the client.
	size := int64(maxInspectBody) + 1
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/size",
		io.LimitReader(repeatReader('x'), size))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // chunked: the router cannot know the size up front

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ServedBy string `json:"served_by"`
		Bytes    int64  `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("bad JSON %q: %v", raw, err)
	}
	if out.Bytes != size {
		t.Errorf("the engine received %d bytes of %d: the body was truncated", out.Bytes, size)
	}
	if out.ServedBy != "alpha" {
		t.Errorf("served_by = %q, want alpha", out.ServedBy)
	}
}

// repeatReader is an endless stream of one byte, so a body larger than the
// inspection cap can be sent without ever holding it in the test.
type repeatReader byte

func (b repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// ?model= is the documented way to route a body too big to sniff: it must
// switch models without the body being read at all.
func TestQueryModelRoutesAnUnsniffableBody(t *testing.T) {
	full := jsonBody("alpha", 256)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model=beta", bytes.NewReader(full))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}

	model, err := requestedModel(r, 64)
	if err != nil {
		t.Fatal(err)
	}
	if model != "beta" {
		t.Errorf("model = %q, want beta from the query string", model)
	}
	if got, _ := io.ReadAll(r.Body); !bytes.Equal(got, full) {
		t.Errorf("the body was touched: %d of %d bytes", len(got), len(full))
	}
}

// ------------------------------------------------------- log_lines

// The settings form presents log_lines as live-editable and the page says a
// save applies at once, but the ring was sized once in NewManager and never
// again: a save asking for 100 lines left the live buffer 4000 wide.
func TestSettingsResizeTheLogBuffer(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	for i := 0; i < 500; i++ {
		mgr.Log().Append(StreamStdout, fmt.Sprintf("line %d", i))
	}

	shown := mgr.Config()
	res, err := mgr.UpdateSettings(SettingsInput{
		MonitorPort: shown.MonitorPort,
		RouterPort:  shown.RouterPort,
		Host:        shown.Host,
		LogLines:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines, _, _ := mgr.Log().Since(0); len(lines) != 100 {
		t.Errorf("the live buffer still holds %d lines, want 100", len(lines))
	}
	// Further output must respect the new size too, not just the trim.
	for i := 0; i < 300; i++ {
		mgr.Log().Append(StreamStdout, "more")
	}
	if lines, _, _ := mgr.Log().Since(0); len(lines) != 100 {
		t.Errorf("the new limit did not hold: %d lines", len(lines))
	}
	var told bool
	for _, n := range res.Notes {
		if strings.Contains(n, "100 lines") {
			told = true
		}
	}
	if !told {
		t.Errorf("the save did not mention the new buffer size: %v", res.Notes)
	}
}

// systemctl reload has to resize the ring too; the file is the other way
// log_lines can change under a running process.
func TestReloadResizesTheLogBuffer(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	for i := 0; i < 500; i++ {
		mgr.Log().Append(StreamStdout, fmt.Sprintf("line %d", i))
	}

	mgr.mu.Lock()
	mgr.cfg.LogLines = 120
	err := mgr.cfg.Save()
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatal(err)
	}
	if lines, _, _ := mgr.Log().Since(0); len(lines) != 120 {
		t.Errorf("after the reload the buffer holds %d lines, want 120", len(lines))
	}
}

// An engine that prints one enormous line used to kill itself on llm-pivot's
// behalf: the log scanner gave up on the line, the pump returned, its pipe
// was closed, and the engine's next write died of SIGPIPE - reported as the
// engine having exited during startup, with nothing pointing at the real
// cause. The engine must survive, and its later output must still arrive.
func TestEngineSurvivesAnEnormousLogLine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		// 250k × len("progress\r") ≈ 2.25 MB with no newline in it.
		e.Env = append(e.Env, "ER_FAKE_HUGE_LINE=250000")
	})
	if err := mgr.Start("alpha"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if logHas(mgr, "still alive after the huge line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("output after the huge line never arrived; state=%+v", mgr.Status())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st := mgr.Status(); st.State != StateRunning {
		t.Errorf("the engine did not survive its own output: %+v", st)
	}
	if !logHas(mgr, "[cut:") {
		t.Error("the line was cut but the log does not say so")
	}
}

// logHas reports whether any buffered line contains want.
func logHas(mgr *Manager, want string) bool {
	lines, _, _ := mgr.Log().Since(0)
	for _, l := range lines {
		if strings.Contains(l.Text, want) {
			return true
		}
	}
	return false
}

// For most of a start there is no process yet - waitPortFree alone can spin
// for seconds - so Status cannot name what is coming up from inst. Without
// the attribution the top bar reads " · — starting…" and no model in the
// tree lights up, which is exactly what a dead engine used to look like.
func TestStartingNamesTheModelBeforeItHasAProcess(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	port := mgr.Config().Engines[0].Port

	// Holding the engine's port keeps the start in waitPortFree, which
	// widens the window that exists on every start into a usable one.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- mgr.Start("alpha") }()

	var st Status
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st = mgr.Status(); st.State == StateStarting {
			break
		}
		if time.Now().After(deadline) {
			ln.Close()
			t.Fatalf("never saw the starting state: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.StartingModel != "alpha" || st.StartingEngine != "fake" {
		t.Errorf("the start is not attributed to alpha: %+v", st)
	}
	if st.Model != "" || st.PID != 0 {
		// Nothing is running yet, and model must keep meaning "running".
		t.Errorf("a process that does not exist was reported as running: %+v", st)
	}

	// Let go of the port so the start can finish.
	ln.Close()
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	st = mgr.Status()
	if st.State != StateRunning || st.Model != "alpha" {
		t.Fatalf("after the start: %+v", st)
	}
	// Once it is running, inst names it and the attribution must go quiet
	// rather than linger as a second, stale answer.
	if st.StartingModel != "" || st.StartingEngine != "" {
		t.Errorf("a running engine is still reported as starting: %+v", st)
	}
}

// ------------------------------------------------- routing the right model

// twoEngineManager gives each model its own engine on its own port, so that
// switching models really does move the traffic to a different listener.
// With one shared engine a misrouted request would land on the right port by
// accident and prove nothing.
func twoEngineManager(t *testing.T) *Manager {
	t.Helper()
	var engines []*Engine
	for _, name := range []string{"alpha", "beta"} {
		engines = append(engines, &Engine{
			Name:       "eng-" + name,
			Port:       freePort(t),
			Command:    fakeEngineCommand(),
			Monitor:    MonitorStdio,
			HealthPath: "/health",
			PortFlag:   "--port",
			Env:        []string{"ER_FAKE_ENGINE=1"},
			Models:     []*Model{{Name: name}},
		})
	}
	cfg := &Config{
		MonitorPort: freePort(t),
		RouterPort:  freePort(t),
		Engines:     engines,
		path:        filepath.Join(t.TempDir(), "config.yaml"),
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// EnsureModel has to report the engine it established. The router used to
// ask separately what was running once it returned, which is a different
// question: by then another request may have swapped the engine, and the
// answer would name a model nobody here asked for.
func TestEnsureModelReportsWhereTheModelRuns(t *testing.T) {
	mgr := twoEngineManager(t)
	portOf := func(engine string) int { return mgr.Config().Engine(engine).Port }

	eng, port, err := mgr.EnsureModel("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if eng != "eng-alpha" || port != portOf("eng-alpha") {
		t.Fatalf("alpha was started on %s:%d, want eng-alpha:%d", eng, port, portOf("eng-alpha"))
	}
	pid := mgr.Status().PID

	// The already-running path must answer the same, without a restart.
	eng, port, err = mgr.EnsureModel("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if eng != "eng-alpha" || port != portOf("eng-alpha") {
		t.Errorf("a running model was reported as %s:%d", eng, port)
	}
	if mgr.Status().PID != pid {
		t.Error("the engine was restarted although it was already serving the model")
	}

	// Switching must report the engine it switched to, not the one it left.
	eng, port, err = mgr.EnsureModel("beta")
	if err != nil {
		t.Fatal(err)
	}
	if eng != "eng-beta" || port != portOf("eng-beta") {
		t.Errorf("beta was reported as %s:%d, want eng-beta:%d", eng, port, portOf("eng-beta"))
	}
}

// Two clients asking for different models at once make the engines swap
// under each other's feet. Whatever that costs - a 502 when the engine is
// killed mid-request, a 503 while one is coming up - a request that gets an
// answer at all must have been answered by the model it asked for.
func TestRouterNeverAnswersWithTheWrongModel(t *testing.T) {
	mgr := twoEngineManager(t)
	srv := httptest.NewServer(NewRouter(mgr))
	defer srv.Close()

	// servedBy returns the engine's --tag, i.e. the model it was started
	// for, or "" for any reply that did not come from an engine.
	servedBy := func(model string) string {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(fmt.Sprintf(`{"model":%q}`, model)))
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return ""
		}
		var out struct {
			ServedBy string `json:"served_by"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Errorf("bad JSON from the router: %q", raw)
			return ""
		}
		return out.ServedBy
	}

	var mu sync.Mutex
	answered := 0
	var wrong []string
	var wg sync.WaitGroup
	for round := 0; round < 8; round++ {
		for _, want := range []string{"alpha", "beta"} {
			wg.Add(1)
			go func(want string) {
				defer wg.Done()
				got := servedBy(want)
				mu.Lock()
				defer mu.Unlock()
				if got == "" {
					return // the engine was swapped away; loud, not wrong
				}
				answered++
				if got != want {
					wrong = append(wrong, fmt.Sprintf("asked %s, answered by %s", want, got))
				}
			}(want)
		}
		wg.Wait()
	}
	if len(wrong) > 0 {
		t.Errorf("%d of %d answers came from the wrong model: %v", len(wrong), answered, wrong)
	}
	if answered == 0 {
		t.Fatal("no request was answered at all, so nothing was checked")
	}
	t.Logf("%d requests answered, all by the model they asked for", answered)
}

// ------------------------------------------------------- throttle bound

// Sweeping stale entries does not bound the throttle on its own: a client
// walking a namespace of bad paths invents keys faster than they go stale,
// and every one of them used to be remembered for ever.
func TestLogThrottleIsBounded(t *testing.T) {
	th := newLogThrottle()
	for i := 0; i < 10000; i++ {
		// A fresh key every time, all well inside the suppression window.
		th.allow(fmt.Sprintf("404 GET /v1/%d", i), time.Hour)
	}
	th.mu.Lock()
	n := len(th.seen)
	th.mu.Unlock()
	if n > maxThrottleKeys {
		t.Errorf("the throttle remembers %d keys, which is past its %d bound", n, maxThrottleKeys)
	}
}

// Bounding it must not break what it is for: a repeated message is still
// suppressed for the window, and a stale one is allowed through again.
func TestLogThrottleStillSuppresses(t *testing.T) {
	th := newLogThrottle()
	if !th.allow("same", time.Hour) {
		t.Fatal("the first message was suppressed")
	}
	if th.allow("same", time.Hour) {
		t.Error("a repeat within the window was not suppressed")
	}
	if !th.allow("same", 0) {
		t.Error("a message past its window was not let through")
	}
	// A key evicted to stay inside the bound simply speaks up again.
	for i := 0; i < 4*maxThrottleKeys; i++ {
		th.allow(fmt.Sprintf("key %d", i), time.Hour)
	}
	if !th.allow("same", time.Hour) {
		t.Error("an evicted key stayed suppressed")
	}
}

// ------------------------------------------------- monitor body on demand

// The sampled body of an http-monitored engine is up to 512 KB and only the
// engine tab displays it, so /api/state leaves it out unless it is asked
// for. The rest of the sample has to travel either way.
func TestStateOmitsTheMonitorBodyUnlessAsked(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Monitor = MonitorHTTP
		e.MonitorURL = "http://127.0.0.1:{{port}}/metrics"
		e.MonitorEvery = 1
	})
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewAPI(mgr, func(string, int, int) {}))
	defer srv.Close()

	get := func(query string) StateResponse {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/state?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out StateResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	deadline := time.Now().Add(10 * time.Second)
	var full StateResponse
	for {
		full = get("since=0&monitor_body=1")
		if full.Status.Monitor != nil && full.Status.Monitor.Body != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no monitor sample arrived: %+v", full.Status.Monitor)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(full.Status.Monitor.Body, "engine_tag") {
		t.Errorf("the body asked for is not the sample: %q", full.Status.Monitor.Body)
	}

	light := get("since=0")
	if light.Status.Monitor == nil {
		t.Fatal("the whole sample went missing, not just its body")
	}
	if light.Status.Monitor.Body != "" {
		t.Errorf("the body travelled unasked: %q", light.Status.Monitor.Body)
	}
	// Everything the page needs for the header must still be there.
	if light.Status.Monitor.URL == "" || light.Status.Monitor.Samples == 0 ||
		light.Status.Monitor.Time == "" {
		t.Errorf("the sample lost more than its body: %+v", light.Status.Monitor)
	}
	// Dropping the body must not disturb the sample the manager holds.
	if again := get("since=0&monitor_body=1"); again.Status.Monitor.Body == "" {
		t.Error("the stored sample lost its body")
	}
}

// ---------------------------------------------------- the {{host}} placeholder

// {{host}} used to be the configured bind address in monitor_url, env and
// work_dir but the engine address in command, so one placeholder said two
// different things depending on where it was written. It is the engine
// address everywhere now, which is the only one that can be reached: an
// engine bound to some other address is one llm-pivot cannot proxy to.
func TestHostPlaceholderIsAlwaysTheEngineAddress(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Monitor = MonitorHTTP
		e.MonitorURL = "http://{{host}}:{{port}}/metrics"
		e.MonitorEvery = 1
	})
	// Bind llm-pivot itself somewhere else entirely: it used to leak into
	// the placeholder, and it must now make no difference at all.
	mgr.mu.Lock()
	mgr.cfg.Host = "0.0.0.0"
	mgr.mu.Unlock()

	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	port := mgr.Config().Engines[0].Port
	want := fmt.Sprintf("http://%s:%d/metrics", engineHost, port)
	if got := mgr.Status().MonitorURL; got != want {
		t.Errorf("monitor_url = %q, want %q", got, want)
	}

	// The proof that it is the right address: the poller reaches it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := mgr.Status().Monitor
		if snap != nil && snap.Status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the monitor URL was never reachable: %+v", snap)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The same placeholder in a command resolves the same way.
	eng := mgr.Config().Engines[0]
	eng.Command = "/bin/true --host {{host}}"
	argv, err := buildArgv(eng, &Model{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if argv[2] != engineHost {
		t.Errorf("--host expanded to %q, want %q", argv[2], engineHost)
	}
}

// ------------------------------------------------- the framed engine page

// monitor: embed used to put monitor_url straight into the frame. That
// address is the engine as llm-pivot reaches it - on llm-pivot's loopback -
// so a browser anywhere else resolved it against its own machine and framed
// that one's port instead. llm-pivot has to fetch the page itself and serve
// it from the origin the browser already has.
func TestEngineFrameIsServedByLlmPivot(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Monitor = MonitorEmbed
		e.MonitorURL = "http://{{host}}:{{port}}/ui/#live"
	})
	srv := httptest.NewServer(NewAPI(mgr, func(string, int, int) {}))
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Nothing is running yet, so there is no page to frame.
	if code, _ := get("/engine/"); code != http.StatusServiceUnavailable {
		t.Errorf("with no engine running /engine/ answered %d, want 503", code)
	}
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}

	code, body := get("/engine/ui/")
	if code != http.StatusOK {
		t.Fatalf("/engine/ui/ answered %d: %s", code, body)
	}
	if !strings.Contains(body, "alpha") {
		t.Errorf("the framed page did not come from the engine: %q", body)
	}
	// The page's relative script resolves under the prefix, which is the
	// whole reason the proxy forwards everything below it.
	code, body = get("/engine/ui/assets/app.js")
	if code != http.StatusOK || !strings.Contains(body, "the framed page's own script") {
		t.Errorf("the page's own script was not reachable: %d %q", code, body)
	}

	// llm-pivot's own page must still be its own.
	if code, body = get("/"); code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Errorf("the monitor page itself broke: %d", code)
	}
	if code, body = get("/app.js"); code != http.StatusOK || strings.Contains(body, "framed page") {
		t.Errorf("/app.js no longer serves llm-pivot's own script: %d", code)
	}
}

// The frame's address is built from monitor_url, so a path and a fragment
// in it still point where they used to - through the proxy.
func TestEngineFrameKeepsThePathAndFragment(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Monitor = MonitorEmbed
		e.MonitorURL = "http://{{host}}:{{port}}/ui/#live"
	})
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	// What the page computes for the frame, and what the proxy maps it to.
	got := mgr.Status().MonitorURL
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/ui/"; u.Path != want || u.Fragment != "live" {
		t.Fatalf("monitor_url = %q, want path %q and fragment \"live\"", got, want)
	}
	target := mgr.EmbedTarget()
	if target == nil {
		t.Fatal("no embed target for a running embed engine")
	}
	if target.Path != "" || target.Host != u.Host {
		t.Errorf("the proxy target is %q, want just the origin of %q", target, got)
	}
}

// Only an engine framing its page gets one proxied: a stdio-monitored one
// has no page, and must not quietly expose its port through llm-pivot.
func TestEngineFrameOnlyForEmbedEngines(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil) // stdio by default
	if err := mgr.Start("alpha"); err != nil {
		t.Fatal(err)
	}
	if target := mgr.EmbedTarget(); target != nil {
		t.Errorf("a stdio-monitored engine offered a frame target: %q", target)
	}
}

// An engine that loads a model binds its port long after its process
// exists, and the frame used to be pointed at it the whole time: llm-pivot
// dialled a port nothing was listening on yet and the frame filled with a
// connection-refused error for the length of the load.
func TestEngineFrameWaitsForTheEngineToAnswer(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Monitor = MonitorEmbed
		e.MonitorURL = "http://{{host}}:{{port}}/ui/#live"
		e.Env = append(e.Env, "ER_FAKE_DELAY_MS=2500") // a slow load
	})
	srv := httptest.NewServer(NewAPI(mgr, func(string, int, int) {}))
	defer srv.Close()

	started := make(chan error, 1)
	go func() { started <- mgr.Start("alpha") }()

	// Catch it while the process is up but the port is not open yet.
	var saw bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := mgr.Status()
		if st.State != StateStarting || st.PID == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		saw = true
		// The page keys the frame on this, so it must say "not yet".
		if st.Healthy {
			t.Fatalf("an engine that has not answered is reported healthy: %+v", st)
		}
		if target := mgr.EmbedTarget(); target != nil {
			t.Errorf("a frame target was offered before the engine answered: %q", target)
		}
		resp, err := http.Get(srv.URL + "/engine/ui/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("/engine/ while starting answered %d, want 503: %s", resp.StatusCode, body)
		}
		if strings.Contains(string(body), "connection refused") {
			t.Errorf("the frame was shown a dial error: %s", body)
		}
		// Whatever it says has to be readable in a frame, which renders it.
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("the frame was sent %q, which a browser renders as it comes", ct)
		}
		if !strings.Contains(string(body), "still starting") {
			t.Errorf("the message does not say what is going on: %s", body)
		}
		break
	}
	if !saw {
		t.Fatal("never caught the engine between spawning and answering")
	}

	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	// And once it answers, the real page comes through.
	resp, err := http.Get(srv.URL + "/engine/ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "alpha") {
		t.Errorf("after the engine answered: %d %s", resp.StatusCode, body)
	}
}
