package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Lifecycle states reported to the monitor page.
const (
	StateIdle     = "idle"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateFailed   = "failed"
)

// engineHost is the address llm-pivot connects to when talking to a
// started engine. Engines are expected to listen on the loopback interface.
const engineHost = "127.0.0.1"

// instance is one running engine process serving one model.
type instance struct {
	engine  string
	model   string
	port    int
	argv    []string
	cmd     *exec.Cmd
	pid     int
	started time.Time

	monitorURL string // resolved {{port}} etc, for http polling or framing

	healthy  bool
	stopping bool
	exitInfo string

	done       chan struct{} // closed once the process has been reaped
	cancelPoll context.CancelFunc
}

func (in *instance) alive() bool {
	select {
	case <-in.done:
		return false
	default:
		return true
	}
}

// MonitorSnapshot is the last sample taken from an http-monitored engine.
type MonitorSnapshot struct {
	URL     string `json:"url"`
	Time    string `json:"time"`
	Status  int    `json:"status"`
	Error   string `json:"error"`
	Body    string `json:"body"`
	Samples int    `json:"samples"`
}

// Status is the runtime half of what the monitor page displays.
type Status struct {
	State       string           `json:"state"`
	Engine      string           `json:"engine"`
	Model       string           `json:"model"`
	Port        int              `json:"port"`
	PID         int              `json:"pid"`
	Command     string           `json:"command"`
	StartedAt   string           `json:"started_at"`
	UptimeSec   int64            `json:"uptime_seconds"`
	Healthy     bool             `json:"healthy"`
	LastError   string           `json:"last_error"`
	MonitorMode string           `json:"monitor_mode"`
	MonitorURL  string           `json:"monitor_url"`
	Monitor     *MonitorSnapshot `json:"monitor"`
	RouterURL   string           `json:"router_url"`
}

// Manager owns the configuration and the single engine process that may run
// at any one time.
type Manager struct {
	opMu sync.Mutex // serialises whole start/stop sequences

	mu      sync.RWMutex
	cfg     *Config
	inst    *instance
	state   string
	lastErr string
	snap    *MonitorSnapshot

	log    *LogBuffer
	client *http.Client
}

func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:    cfg,
		state:  StateIdle,
		log:    NewLogBuffer(cfg.LogLines),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Log exposes the shared output buffer.
func (m *Manager) Log() *LogBuffer { return m.log }

// Config returns a snapshot of the current configuration.
func (m *Manager) Config() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone()
}

// Status describes what is running right now.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{
		State:     m.state,
		LastError: m.lastErr,
		Monitor:   m.snap,
		RouterURL: fmt.Sprintf("http://%s:%d", m.cfg.Host, m.cfg.RouterPort),
	}
	if in := m.inst; in != nil {
		st.Engine = in.engine
		st.Model = in.model
		st.Port = in.port
		st.PID = in.pid
		st.Command = QuoteArgs(in.argv)
		st.StartedAt = in.started.Format(time.RFC3339)
		st.UptimeSec = int64(time.Since(in.started).Seconds())
		st.Healthy = in.healthy
		st.MonitorURL = in.monitorURL
		if e := m.cfg.Engine(in.engine); e != nil {
			st.MonitorMode = e.MonitorMode()
		}
	}
	return st
}

// RunningTarget describes what the router should forward to. model is ""
// when nothing is running.
func (m *Manager) RunningTarget() (engine, model string, port int, ready bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.inst == nil || !m.inst.alive() {
		return "", "", 0, false
	}
	return m.inst.engine, m.inst.model, m.inst.port, m.inst.healthy
}

// ---------------------------------------------------------------- lifecycle

// Start runs the named model, replacing whatever is running.
func (m *Manager) Start(model string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.startLocked(model)
}

// Restart stops and starts the named model (or the running one when empty).
func (m *Manager) Restart(model string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if model == "" {
		m.mu.RLock()
		if m.inst != nil {
			model = m.inst.model
		}
		m.mu.RUnlock()
	}
	if model == "" {
		return fmt.Errorf("nothing is running: select a model to start")
	}
	m.stopLocked("restart")
	return m.startLocked(model)
}

// Stop terminates the running engine, if any.
func (m *Manager) Stop() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.stopLocked("stop requested")
	return nil
}

// EnsureModel is the entry point used by the router: it is a no-op when the
// requested model is already being served, and otherwise swaps engines.
func (m *Manager) EnsureModel(model string) error {
	m.mu.RLock()
	ok := m.inst != nil && m.inst.model == model && m.inst.healthy && m.inst.alive()
	m.mu.RUnlock()
	if ok {
		return nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	// Re-check: another request may have started it while we waited.
	m.mu.RLock()
	ok = m.inst != nil && m.inst.model == model && m.inst.healthy && m.inst.alive()
	m.mu.RUnlock()
	if ok {
		return nil
	}
	return m.startLocked(model)
}

// startLocked must be called with opMu held.
func (m *Manager) startLocked(model string) error {
	m.mu.RLock()
	cfg := m.cfg
	eng, mdl := cfg.FindModel(model)
	host := cfg.Host
	globalTimeout := cfg.StartupTimeout
	m.mu.RUnlock()

	if eng == nil {
		return fmt.Errorf("unknown model %q", model)
	}

	m.stopLocked("starting " + model)

	argv, err := buildArgv(eng, mdl)
	if err != nil {
		m.fail(fmt.Sprintf("cannot build command for model %q: %v", model, err))
		return err
	}

	timeout := globalTimeout
	if eng.StartupTimeout > 0 {
		timeout = eng.StartupTimeout
	}

	m.mu.Lock()
	m.state = StateStarting
	m.lastErr = ""
	m.snap = nil
	m.mu.Unlock()

	// Refuse to start on top of something that already holds the port.
	if err := waitPortFree(eng.Port, 3*time.Second); err != nil {
		m.log.System("✗ " + err.Error())
		m.fail(err.Error())
		return err
	}

	m.log.System(fmt.Sprintf("── starting engine %q with model %q on port %d", eng.Name, mdl.Name, eng.Port))
	m.log.System("$ " + QuoteArgs(argv))

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = expandVars(eng.WorkDir, eng, mdl, host)
	cmd.Env = os.Environ()
	for _, kv := range eng.Env {
		cmd.Env = append(cmd.Env, expandVars(kv, eng, mdl, host))
	}
	// Own process group, so the whole engine tree can be signalled at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	outR, outW, err := os.Pipe()
	if err != nil {
		m.fail(err.Error())
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		m.fail(err.Error())
		return err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		msg := fmt.Sprintf("cannot start engine %q: %v", eng.Name, err)
		m.log.System(msg)
		m.fail(msg)
		return fmt.Errorf("%s", msg)
	}
	// The child holds its own copies now.
	outW.Close()
	errW.Close()

	in := &instance{
		engine:  eng.Name,
		model:   mdl.Name,
		port:    eng.Port,
		argv:    argv,
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		started: time.Now(),
		done:    make(chan struct{}),
	}
	if eng.NeedsMonitorURL() {
		in.monitorURL = expandVars(eng.MonitorURL, eng, mdl, host)
	}

	go m.log.pump(StreamStdout, outR)
	go m.log.pump(StreamStderr, errR)

	// The poll context is installed before the reaper starts, because the
	// reaper cancels it once the process is gone.
	var pollCtx context.Context
	if eng.MonitorMode() == MonitorHTTP {
		var cancel context.CancelFunc
		pollCtx, cancel = context.WithCancel(context.Background())
		in.cancelPoll = cancel
	}

	m.mu.Lock()
	m.inst = in
	m.mu.Unlock()

	go m.reap(in)
	if pollCtx != nil {
		go m.pollMonitor(pollCtx, in, in.monitorURL, eng.PollInterval())
	}

	if err := m.waitHealthy(in, eng.HealthPath, time.Duration(timeout)*time.Second); err != nil {
		m.log.System("✗ " + err.Error())
		m.stopLocked("startup failed")
		m.fail(err.Error())
		return err
	}

	m.mu.Lock()
	in.healthy = true
	if m.inst == in {
		m.state = StateRunning
	}
	m.mu.Unlock()
	m.log.System(fmt.Sprintf("✓ engine %q is serving model %q on port %d (pid %d)",
		in.engine, in.model, in.port, in.pid))
	return nil
}

// reap waits for the process and records how it ended.
func (m *Manager) reap(in *instance) {
	err := in.cmd.Wait()
	m.mu.Lock()
	switch {
	case err == nil:
		in.exitInfo = "exited normally"
	default:
		in.exitInfo = err.Error()
	}
	wasCurrent := m.inst == in
	intentional := in.stopping
	m.mu.Unlock()

	close(in.done)
	if in.cancelPoll != nil {
		in.cancelPoll()
	}

	if intentional {
		m.log.System(fmt.Sprintf("engine %q stopped (%s)", in.engine, in.exitInfo))
		return
	}
	m.log.System(fmt.Sprintf("✗ engine %q for model %q died unexpectedly: %s",
		in.engine, in.model, in.exitInfo))
	if wasCurrent {
		m.mu.Lock()
		if m.inst == in {
			m.inst = nil
			m.state = StateFailed
			m.lastErr = fmt.Sprintf("engine %q for model %q died: %s", in.engine, in.model, in.exitInfo)
		}
		m.mu.Unlock()
	}
}

// stopLocked terminates the current instance. opMu must be held.
func (m *Manager) stopLocked(reason string) {
	m.mu.Lock()
	in := m.inst
	if in == nil {
		if m.state != StateFailed {
			m.state = StateIdle
		}
		m.mu.Unlock()
		return
	}
	in.stopping = true
	m.state = StateStopping
	stopTimeout := time.Duration(m.cfg.StopTimeout) * time.Second
	m.mu.Unlock()

	if in.alive() {
		m.log.System(fmt.Sprintf("── stopping engine %q (model %q, pid %d): %s",
			in.engine, in.model, in.pid, reason))
		signalGroup(in.pid, syscall.SIGTERM)
		select {
		case <-in.done:
		case <-time.After(stopTimeout):
			m.log.System(fmt.Sprintf("engine %q did not exit within %s, killing it",
				in.engine, stopTimeout))
			signalGroup(in.pid, syscall.SIGKILL)
			select {
			case <-in.done:
			case <-time.After(5 * time.Second):
				m.log.System(fmt.Sprintf("engine %q is unresponsive to SIGKILL", in.engine))
			}
		}
	}
	if in.cancelPoll != nil {
		in.cancelPoll()
	}

	m.mu.Lock()
	if m.inst == in {
		m.inst = nil
		m.state = StateIdle
		m.snap = nil
	}
	m.mu.Unlock()
}

// signalGroup signals the whole process group of pid, falling back to the
// single process when the group is already gone.
func signalGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

func (m *Manager) fail(msg string) {
	m.mu.Lock()
	m.state = StateFailed
	m.lastErr = msg
	m.mu.Unlock()
}

// waitHealthy blocks until the engine answers on its port, the process dies,
// or the timeout expires.
func (m *Manager) waitHealthy(in *instance, healthPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(engineHost, strconv.Itoa(in.port))
	probe := &http.Client{Timeout: 3 * time.Second}
	announced := false

	for {
		if !in.alive() {
			return fmt.Errorf("engine %q exited during startup (%s); see the log above",
				in.engine, in.exitInfo)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("engine %q did not answer on port %d within %s; "+
				"if this engine needs longer to come up, raise its startup timeout "+
				"(the engine's own startup_timeout_seconds, or the global one in Settings)",
				in.engine, in.port, timeout)
		}
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			if healthPath == "" {
				return nil
			}
			if !announced {
				announced = true
				m.log.System(fmt.Sprintf("port %d is open, waiting for %s", in.port, healthPath))
			}
			resp, err := probe.Get("http://" + addr + healthPath)
			if err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					return nil
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// waitPortFree checks that nothing is already listening on the engine's port,
// allowing a short grace for the previous engine's socket to go away.
//
// Without this, starting on top of a stranded engine from an earlier manual
// run looks like a perfectly successful start: the health probe is answered
// by the stranger, and requests are then served by the wrong model.
func waitPortFree(port int, grace time.Duration) error {
	addr := net.JoinHostPort(engineHost, strconv.Itoa(port))
	deadline := time.Now().Add(grace)
	var last error
	for {
		// Probe by binding, not by connecting. "Can something accept a
		// connection here" is the wrong question: a listener whose accept
		// queue is full makes connect time out, which is indistinguishable
		// from an unused port. "Can the engine bind this port" is exactly
		// what we need to know, and the kernel answers it directly.
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d is already in use by another process, so llm-pivot "+
				"will not start on top of it (requests would reach the wrong engine); stop what "+
				"is listening there - ss -ltnp 'sport = :%d' - or give the engine another port [%v]",
				port, port, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// pollMonitor samples an http-monitored engine until the instance stops.
func (m *Manager) pollMonitor(ctx context.Context, in *instance, url string, every int) {
	t := time.NewTicker(time.Duration(every) * time.Second)
	defer t.Stop()
	samples := 0
	sample := func() {
		snap := &MonitorSnapshot{URL: url, Time: time.Now().Format("15:04:05")}
		samples++
		snap.Samples = samples
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			snap.Error = err.Error()
		} else if resp, err := m.client.Do(req); err != nil {
			snap.Error = err.Error()
		} else {
			snap.Status = resp.StatusCode
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
			resp.Body.Close()
			snap.Body = string(body)
		}
		m.mu.Lock()
		if m.inst == in {
			m.snap = snap
		}
		m.mu.Unlock()
	}
	sample()
	for {
		select {
		case <-ctx.Done():
			return
		case <-in.done:
			return
		case <-t.C:
			sample()
		}
	}
}

// Shutdown stops the engine when llm-pivot itself exits.
func (m *Manager) Shutdown() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.stopLocked("llm-pivot is shutting down")
}

// ------------------------------------------------------------- command line

// buildArgv produces the final argument vector for an engine/model pair.
func buildArgv(e *Engine, mdl *Model) ([]string, error) {
	base, err := SplitArgs(expandVars(e.Command, e, mdl, ""))
	if err != nil {
		return nil, fmt.Errorf("engine command: %w", err)
	}
	if len(base) == 0 {
		return nil, fmt.Errorf("engine command is empty")
	}
	over, err := SplitArgs(expandVars(mdl.Options, e, mdl, ""))
	if err != nil {
		return nil, fmt.Errorf("model options: %w", err)
	}
	prog, baseArgs := base[0], base[1:]
	args := MergeArgs(baseArgs, over)

	// Make sure the engine really listens on the port we will proxy to.
	if e.PortFlag != "" && e.PortFlag != "-" && !HasFlag(args, e.PortFlag) {
		args = append(args, e.PortFlag, strconv.Itoa(e.Port))
	}
	return append([]string{prog}, args...), nil
}

// expandVars substitutes the {{...}} placeholders usable in commands,
// options, monitor URLs, env entries and working directories.
func expandVars(s string, e *Engine, mdl *Model, host string) string {
	if s == "" || !strings.Contains(s, "{{") {
		return s
	}
	r := strings.NewReplacer(
		"{{port}}", strconv.Itoa(e.Port),
		"{{engine}}", e.Name,
		"{{model}}", modelName(mdl),
		"{{name}}", modelName(mdl),
		"{{host}}", hostOr(host, engineHost),
		"{{home}}", os.Getenv("HOME"),
	)
	return r.Replace(s)
}

func modelName(m *Model) string {
	if m == nil {
		return ""
	}
	return m.Name
}

func hostOr(h, def string) string {
	if h == "" {
		return def
	}
	return h
}
