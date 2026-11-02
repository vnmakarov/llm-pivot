package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
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
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"served_by":%q,"echo":%s}`, tag, string(body))
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
