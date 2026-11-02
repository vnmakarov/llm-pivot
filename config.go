package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults used when the configuration file omits a value.
const (
	DefaultMonitorPort         = 8061
	DefaultRouterPort          = 8078
	DefaultHost                = "127.0.0.1"
	DefaultStartupTimeout      = 180
	DefaultGracefulStopTimeout = 20
	DefaultInactiveStopTimeout = 600
	DefaultMonitorPoll         = 5
	DefaultPortFlag            = "--port"
	DefaultLogLines            = 4000
)

// Monitoring sources understood by the router.
const (
	MonitorStdio = "stdio" // read the engine's stdout/stderr
	MonitorHTTP  = "http"  // poll an http endpoint exposed by the engine
	MonitorEmbed = "embed" // show the engine's own monitoring page in a frame
)

// Model is a single model served by an engine. Options are extra engine
// arguments; flags given here replace the same flags in the engine command.
type Model struct {
	Name    string `yaml:"name" json:"name"`
	Options string `yaml:"options,omitempty" json:"options"`
}

// Engine is one local LLM server program (llama.cpp, strata, ninfer, ...).
type Engine struct {
	Name    string `yaml:"name" json:"name"`
	Port    int    `yaml:"port" json:"port"`
	Command string `yaml:"command" json:"command"`

	// Monitoring: "stdio" (default), "http" or "embed".
	Monitor      string `yaml:"monitor,omitempty" json:"monitor"`
	MonitorURL   string `yaml:"monitor_url,omitempty" json:"monitor_url"`
	MonitorEvery int    `yaml:"monitor_interval_seconds,omitempty" json:"monitor_interval_seconds"`

	// HealthPath is requested while waiting for the engine to come up.
	// Empty means "a successful TCP connect is enough".
	HealthPath string `yaml:"health_path,omitempty" json:"health_path"`

	// PortFlag is appended with the engine port unless the command already
	// sets it. Set it to "-" to never inject a port flag.
	PortFlag string `yaml:"port_flag,omitempty" json:"port_flag"`

	// StartupTimeout overrides the global startup timeout for this engine.
	StartupTimeout int `yaml:"startup_timeout_seconds,omitempty" json:"startup_timeout_seconds"`

	// Env holds extra KEY=VALUE entries added to the engine environment.
	Env []string `yaml:"env,omitempty" json:"env"`

	// WorkDir is the working directory of the engine process.
	WorkDir string `yaml:"work_dir,omitempty" json:"work_dir"`

	Models []*Model `yaml:"models" json:"models"`
}

// Config is the whole persisted state of llm-pivot.
type Config struct {
	MonitorPort         int       `yaml:"monitor_port" json:"monitor_port"`
	RouterPort          int       `yaml:"router_port" json:"router_port"`
	Host                string    `yaml:"host,omitempty" json:"host"`
	StartupTimeout      int       `yaml:"startup_timeout_seconds,omitempty" json:"startup_timeout_seconds"`
	GracefulStopTimeout int       `yaml:"graceful_stop_timeout_seconds,omitempty" json:"graceful_stop_timeout_seconds"`
	InactiveStopTimeout int       `yaml:"inactive_stop_timeout_seconds,omitempty" json:"inactive_stop_timeout_seconds"`
	LogLines            int       `yaml:"log_lines,omitempty" json:"log_lines"`
	Engines             []*Engine `yaml:"engines" json:"engines"`

	// A command-line flag may shadow the three keys above that say where to
	// listen. The exported fields always hold the effective value - the file
	// with the flag layered on top - because that is what the listeners, the
	// page and the port checks have to see. file and flag keep the two
	// layers apart so that Save() can write the file's own value back
	// (README: "Overrides are not written back to the file") and Reload()
	// can put the flag on again after re-reading the file.
	file, flag listeners

	path string `yaml:"-" json:"-"`
}

// listeners is one layer of the three keys that decide where llm-pivot
// listens. A zero port or an empty host means "this layer says nothing".
type listeners struct {
	MonitorPort int
	RouterPort  int
	Host        string
}

// Clone returns a deep copy, so the HTTP layer can hand out or edit a
// configuration without racing the running router.
func (c *Config) Clone() *Config {
	out := &Config{
		MonitorPort:         c.MonitorPort,
		RouterPort:          c.RouterPort,
		Host:                c.Host,
		StartupTimeout:      c.StartupTimeout,
		GracefulStopTimeout: c.GracefulStopTimeout,
		InactiveStopTimeout: c.InactiveStopTimeout,
		LogLines:            c.LogLines,
		file:                c.file,
		flag:                c.flag,
		path:                c.path,
	}
	for _, e := range c.Engines {
		ne := *e
		ne.Env = append([]string(nil), e.Env...)
		ne.Models = nil
		for _, m := range e.Models {
			nm := *m
			ne.Models = append(ne.Models, &nm)
		}
		out.Engines = append(out.Engines, &ne)
	}
	return out
}

// Engine returns the engine with the given name, or nil.
func (c *Config) Engine(name string) *Engine {
	for _, e := range c.Engines {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// FindModel locates a model by its (globally unique) name.
func (c *Config) FindModel(name string) (*Engine, *Model) {
	for _, e := range c.Engines {
		for _, m := range e.Models {
			if m.Name == name {
				return e, m
			}
		}
	}
	return nil, nil
}

// Model returns the model of a specific engine, or nil.
func (e *Engine) Model(name string) *Model {
	for _, m := range e.Models {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// MonitorMode returns the normalised monitoring source of the engine.
func (e *Engine) MonitorMode() string {
	switch e.Monitor {
	case MonitorHTTP, MonitorEmbed:
		return e.Monitor
	default:
		return MonitorStdio
	}
}

// NeedsMonitorURL reports whether the monitoring mode requires a URL.
func (e *Engine) NeedsMonitorURL() bool {
	m := e.MonitorMode()
	return m == MonitorHTTP || m == MonitorEmbed
}

// PollInterval is how often an http-monitored engine is sampled.
func (e *Engine) PollInterval() int {
	if e.MonitorEvery > 0 {
		return e.MonitorEvery
	}
	return DefaultMonitorPoll
}

// Overrides layers the -monitor-port, -router-port and -host flags on top of
// what the file said. A zero port or an empty host means the flag was not
// given. The overrides stay with the configuration: they survive Clone, they
// are re-applied after every Reload, and Save keeps them out of the file.
func (c *Config) Overrides(monitorPort, routerPort int, host string) {
	c.flag = listeners{MonitorPort: monitorPort, RouterPort: routerPort, Host: strings.TrimSpace(host)}
	c.normalize()
}

// adoptOverrides carries the command-line flags of one configuration over to
// its replacement, which is what a reload re-reading the file needs.
func (c *Config) adoptOverrides(from *Config) {
	c.flag = from.flag
	c.normalize()
}

// overridden names the keys a command-line flag currently shadows, so that
// the page and the log can say why the file and the live value differ.
func (c *Config) overridden() []string {
	var out []string
	if c.flag.MonitorPort != 0 && c.flag.MonitorPort != c.file.MonitorPort {
		out = append(out, "monitor_port")
	}
	if c.flag.RouterPort != 0 && c.flag.RouterPort != c.file.RouterPort {
		out = append(out, "router_port")
	}
	if c.flag.Host != "" && c.flag.Host != c.file.Host {
		out = append(out, "host")
	}
	return out
}

// SetListeners records the three keys the settings page can change. A value
// that differs from the one in force also drops a command-line override of
// that key, because the page must not silently ignore what was typed into
// it; the note returned for each says so. A value equal to the one in force
// says nothing, so saving the form untouched keeps the overrides and leaves
// the file's own values alone - which is what makes them non-sticky.
func (c *Config) SetListeners(monitorPort, routerPort int, host string) []string {
	var notes []string
	// was is the flag's own value, empty when no flag shadowed this key.
	drop := func(key, was string) {
		if was != "" {
			notes = append(notes, fmt.Sprintf("-%s %s no longer applies: the page set %s itself.",
				strings.ReplaceAll(key, "_", "-"), was, key))
		}
	}
	if monitorPort > 0 && monitorPort != c.MonitorPort {
		drop("monitor_port", flagged(c.flag.MonitorPort))
		c.file.MonitorPort, c.flag.MonitorPort = monitorPort, 0
	}
	if routerPort > 0 && routerPort != c.RouterPort {
		drop("router_port", flagged(c.flag.RouterPort))
		c.file.RouterPort, c.flag.RouterPort = routerPort, 0
	}
	if host = strings.TrimSpace(host); host != "" && host != c.Host {
		drop("host", c.flag.Host)
		c.file.Host, c.flag.Host = host, ""
	}
	c.normalize()
	return notes
}

// flagged renders a port a flag set, or "" when the flag was not given.
func flagged(port int) string {
	if port == 0 {
		return ""
	}
	return strconv.Itoa(port)
}

// normalize fills in defaults and canonicalises user-written values.
// It is called both after loading and before saving.
func (c *Config) normalize() {
	// Defaults belong to the file's own layer, so that a flag is never the
	// thing that gets defaulted and never the thing that gets written back.
	// An empty layer adopts the exported fields first: that is how a freshly
	// decoded file - and a Config built in code - gets its layer filled in.
	if c.file.MonitorPort == 0 {
		c.file.MonitorPort = c.MonitorPort
	}
	if c.file.RouterPort == 0 {
		c.file.RouterPort = c.RouterPort
	}
	if strings.TrimSpace(c.file.Host) == "" {
		c.file.Host = strings.TrimSpace(c.Host)
	}
	if c.file.MonitorPort == 0 {
		c.file.MonitorPort = DefaultMonitorPort
	}
	if c.file.RouterPort == 0 {
		c.file.RouterPort = DefaultRouterPort
	}
	if c.file.Host == "" {
		c.file.Host = DefaultHost
	}

	// The effective values the rest of the program reads.
	c.MonitorPort, c.RouterPort, c.Host = c.file.MonitorPort, c.file.RouterPort, c.file.Host
	if c.flag.MonitorPort != 0 {
		c.MonitorPort = c.flag.MonitorPort
	}
	if c.flag.RouterPort != 0 {
		c.RouterPort = c.flag.RouterPort
	}
	if c.flag.Host != "" {
		c.Host = c.flag.Host
	}

	if c.StartupTimeout <= 0 {
		c.StartupTimeout = DefaultStartupTimeout
	}
	if c.GracefulStopTimeout <= 0 {
		c.GracefulStopTimeout = DefaultGracefulStopTimeout
	}
	// A negative value is kept: it means "never stop an idle engine".
	if c.InactiveStopTimeout == 0 {
		c.InactiveStopTimeout = DefaultInactiveStopTimeout
	}
	if c.LogLines <= 0 {
		c.LogLines = DefaultLogLines
	}
	for _, e := range c.Engines {
		e.Name = strings.TrimSpace(e.Name)
		e.Command = strings.TrimSpace(e.Command)
		e.Monitor = normalizeMonitor(e.Monitor)
		e.MonitorURL = strings.TrimSpace(e.MonitorURL)
		e.HealthPath = strings.TrimSpace(e.HealthPath)
		e.PortFlag = strings.TrimSpace(e.PortFlag)
		if e.PortFlag == "" {
			e.PortFlag = DefaultPortFlag
		}
		for _, m := range e.Models {
			m.Name = strings.TrimSpace(m.Name)
			m.Options = strings.TrimSpace(m.Options)
		}
	}
}

// normalizeMonitor accepts the spellings a user is likely to write for the
// two monitoring sources and maps them onto the canonical values.
func normalizeMonitor(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", MonitorStdio, "stdout", "stderr", "stdout/stderr", "stderr/stdout", "output", "log", "logs":
		return MonitorStdio
	case MonitorHTTP, "https", "url", "port":
		return MonitorHTTP
	case MonitorEmbed, "iframe", "frame", "page", "web":
		return MonitorEmbed
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// Validate reports the first problem that makes the configuration unusable.
// Its messages are shown verbatim to the user when the program refuses to
// start, so they name the offending engine/model.
func (c *Config) Validate() error {
	if err := validPort("monitor_port", c.MonitorPort); err != nil {
		return err
	}
	if err := validPort("router_port", c.RouterPort); err != nil {
		return err
	}
	if c.MonitorPort == c.RouterPort {
		return fmt.Errorf("monitor_port and router_port are both %d: they must differ", c.MonitorPort)
	}

	engineNames := map[string]bool{}
	modelOwner := map[string]string{}
	for i, e := range c.Engines {
		where := fmt.Sprintf("engine #%d", i+1)
		if e.Name == "" {
			return fmt.Errorf("%s has an empty name", where)
		}
		where = fmt.Sprintf("engine %q", e.Name)
		if engineNames[e.Name] {
			return fmt.Errorf("duplicated engine name %q", e.Name)
		}
		engineNames[e.Name] = true

		if err := validPort(where+" port", e.Port); err != nil {
			return err
		}
		if e.Port == c.MonitorPort {
			return fmt.Errorf("%s uses port %d which is the monitor_port", where, e.Port)
		}
		if e.Port == c.RouterPort {
			return fmt.Errorf("%s uses port %d which is the router_port", where, e.Port)
		}
		if e.Command == "" {
			return fmt.Errorf("%s has an empty command", where)
		}
		argv, err := SplitArgs(e.Command)
		if err != nil {
			return fmt.Errorf("%s has a malformed command: %w", where, err)
		}
		if len(argv) == 0 {
			return fmt.Errorf("%s has an empty command", where)
		}
		switch e.Monitor {
		case MonitorStdio:
		case MonitorHTTP, MonitorEmbed:
			if e.MonitorURL == "" {
				return fmt.Errorf("%s uses monitor: %s but has no monitor_url", where, e.Monitor)
			}
		default:
			return fmt.Errorf("%s has unknown monitor %q (expected %q, %q or %q)",
				where, e.Monitor, MonitorStdio, MonitorHTTP, MonitorEmbed)
		}
		if e.HealthPath != "" && !strings.HasPrefix(e.HealthPath, "/") {
			return fmt.Errorf("%s has health_path %q which must start with %q", where, e.HealthPath, "/")
		}
		for _, kv := range e.Env {
			if !strings.Contains(kv, "=") {
				return fmt.Errorf("%s has env entry %q which is not KEY=VALUE", where, kv)
			}
		}

		for j, m := range e.Models {
			if m.Name == "" {
				return fmt.Errorf("%s has a model #%d with an empty name", where, j+1)
			}
			if owner, dup := modelOwner[m.Name]; dup {
				if owner == e.Name {
					return fmt.Errorf("duplicated model name %q in engine %q", m.Name, e.Name)
				}
				return fmt.Errorf("duplicated model name %q: used by engine %q and engine %q "+
					"(model names must be unique across all engines)", m.Name, owner, e.Name)
			}
			modelOwner[m.Name] = e.Name
			if _, err := SplitArgs(m.Options); err != nil {
				return fmt.Errorf("%s model %q has malformed options: %w", where, m.Name, err)
			}
		}
	}
	return nil
}

func validPort(what string, p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s is %d which is not a valid TCP port (1-65535)", what, p)
	}
	return nil
}

// LoadConfig reads and validates the configuration file. Unknown options,
// malformed YAML and duplicated model names are all reported as errors.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{path: path}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown options
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the configuration back atomically.
func (c *Config) Save() error {
	c.normalize()
	// Write the file's own values for the three keys a flag may shadow: an
	// override is a property of this run, not of the file (README:
	// "Overrides are not written back to the file"). The copy is shallow
	// because marshalling only reads the engines.
	onFile := *c
	onFile.MonitorPort, onFile.RouterPort, onFile.Host = c.file.MonitorPort, c.file.RouterPort, c.file.Host
	data, err := yaml.Marshal(&onFile)
	if err != nil {
		return err
	}
	header := "# llm-pivot configuration.\n" +
		"# Rewritten by the monitor page; hand-written comments are not preserved.\n"
	data = append([]byte(header), data...)

	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

// Path is the file this configuration was loaded from.
func (c *Config) Path() string { return c.path }

// defaultConfigYAML is written when no configuration file exists yet. It is a
// literal so that the explanatory comments survive the first start.
const defaultConfigYAML = `# llm-pivot configuration.
#
# monitor_port  port of the monitoring / settings web page
# router_port   OpenAI-compatible port forwarded to the running engine
monitor_port: 8061
router_port: 8078
host: 127.0.0.1

# How long to wait for a freshly started engine to answer, how long to
# wait for a stopping engine to exit gracefully before killing it, and how long
# an engine may idle before being stopped automatically.
startup_timeout_seconds: 180
graceful_stop_timeout_seconds: 20
inactive_stop_timeout_seconds: 600

engines:
  - name: llama.cpp
    port: 8080
    # Flags given here are the defaults for every model of this engine.
    # A model may repeat a flag to override its value.
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
    # Monitoring info is taken from the engine's stdout/stderr.
    monitor: stdio
    health_path: /health
    models: []
`

// WriteDefaultConfig creates path with the stock configuration.
func WriteDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigYAML), 0o644)
}
