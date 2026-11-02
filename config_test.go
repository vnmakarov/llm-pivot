package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigRejectsUnknownOption(t *testing.T) {
	p := writeCfg(t, `
monitor_port: 8061
router_port: 8078
engines:
  - name: e
    port: 8080
    command: /bin/true
    wobble: yes
`)
	_, err := LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "wobble") {
		t.Fatalf("expected an unknown-option error, got %v", err)
	}
}

func TestLoadConfigRejectsDuplicateModelsAcrossEngines(t *testing.T) {
	p := writeCfg(t, `
engines:
  - name: a
    port: 8080
    command: /bin/true
    models:
      - name: shared
  - name: b
    port: 8081
    command: /bin/true
    models:
      - name: shared
`)
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatal("expected a duplicate-model error")
	}
	for _, want := range []string{"shared", "unique"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestLoadConfigRejectsDuplicateModelsInOneEngine(t *testing.T) {
	p := writeCfg(t, `
engines:
  - name: a
    port: 8080
    command: /bin/true
    models:
      - name: dup
      - name: dup
`)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("expected a duplicate-model error, got %v", err)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"clashing ports": `
monitor_port: 8061
router_port: 8061
engines: []
`,
		"engine on the router port": `
monitor_port: 8061
router_port: 8078
engines:
  - name: a
    port: 8078
    command: /bin/true
`,
		"unknown monitor": `
engines:
  - name: a
    port: 8080
    command: /bin/true
    monitor: carrier-pigeon
`,
		"http monitor without url": `
engines:
  - name: a
    port: 8080
    command: /bin/true
    monitor: http
`,
		"malformed command": `
engines:
  - name: a
    port: 8080
    command: '/bin/true --msg "oops'
`,
		"duplicate engine name": `
engines:
  - name: a
    port: 8080
    command: /bin/true
  - name: a
    port: 8081
    command: /bin/true
`,
		"empty model name": `
engines:
  - name: a
    port: 8080
    command: /bin/true
    models:
      - name: ""
`,
	}
	for what, body := range cases {
		if _, err := LoadConfig(writeCfg(t, body)); err == nil {
			t.Errorf("%s: expected an error", what)
		} else {
			t.Logf("%s -> %v", what, err)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	p := writeCfg(t, "engines: []\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MonitorPort != DefaultMonitorPort || c.RouterPort != DefaultRouterPort {
		t.Errorf("ports = %d/%d, want %d/%d",
			c.MonitorPort, c.RouterPort, DefaultMonitorPort, DefaultRouterPort)
	}
	if c.Host != DefaultHost {
		t.Errorf("host = %q", c.Host)
	}
}

func TestMonitorSpellings(t *testing.T) {
	for _, s := range []string{"", "stdio", "stderr", "stdout", "stderr/stdout", "LOGS"} {
		if got := normalizeMonitor(s); got != MonitorStdio {
			t.Errorf("normalizeMonitor(%q) = %q, want stdio", s, got)
		}
	}
	for _, s := range []string{"http", "HTTP", "url"} {
		if got := normalizeMonitor(s); got != MonitorHTTP {
			t.Errorf("normalizeMonitor(%q) = %q, want http", s, got)
		}
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefaultConfig(p); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("the shipped default configuration must load: %v", err)
	}
	if len(c.Engines) != 1 || c.Engines[0].Name != "llama.cpp" {
		t.Fatalf("unexpected default engines: %+v", c.Engines)
	}
	argv, err := buildArgv(c.Engines[0], &Model{Name: "m", Options: "-m /x.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	line := QuoteArgs(argv)
	for _, want := range []string{
		"llama-server", "--models-max 1", "--device Vulkan1",
		"'Thinking time expired. Formulate your final response now.'",
		"-m /x.gguf", "--port 8080",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("command line is missing %q:\n%s", want, line)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefaultConfig(p); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	c.Engines[0].Models = append(c.Engines[0].Models, &Model{Name: "m1", Options: "-m /a.gguf"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("a saved configuration must load back: %v", err)
	}
	if len(again.Engines[0].Models) != 1 || again.Engines[0].Models[0].Name != "m1" {
		t.Fatalf("round trip lost the model: %+v", again.Engines[0].Models)
	}
	if again.Engines[0].Command != c.Engines[0].Command {
		t.Errorf("round trip changed the command:\n%q\n%q",
			again.Engines[0].Command, c.Engines[0].Command)
	}
}

func TestCloneIsDeep(t *testing.T) {
	c := &Config{Engines: []*Engine{{Name: "a", Models: []*Model{{Name: "m"}}}}}
	d := c.Clone()
	d.Engines[0].Name = "b"
	d.Engines[0].Models[0].Name = "n"
	if c.Engines[0].Name != "a" || c.Engines[0].Models[0].Name != "m" {
		t.Error("Clone shares state with the original")
	}
}

func TestEmbedMonitorMode(t *testing.T) {
	p := writeCfg(t, `
engines:
  - name: strata
    port: 8024
    command: /bin/true
    monitor: embed
    monitor_url: http://127.0.0.1:{{port}}/#monitor
`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Engines[0]
	if e.MonitorMode() != MonitorEmbed {
		t.Errorf("MonitorMode = %q, want embed", e.MonitorMode())
	}
	if !e.NeedsMonitorURL() {
		t.Error("embed mode needs a url")
	}
	// The fragment must survive: it is what selects the page's own tab.
	if got := expandVars(e.MonitorURL, e, &Model{Name: "m"}, ""); got != "http://127.0.0.1:8024/#monitor" {
		t.Errorf("expanded url = %q", got)
	}

	// embed without a url is refused, like http without one.
	bad := writeCfg(t, `
engines:
  - name: s
    port: 8024
    command: /bin/true
    monitor: embed
`)
	if _, err := LoadConfig(bad); err == nil || !strings.Contains(err.Error(), "monitor_url") {
		t.Errorf("got %v, want a monitor_url complaint", err)
	}
}

func TestEmbedSpellings(t *testing.T) {
	for _, s := range []string{"embed", "iframe", "frame", "page", "WEB"} {
		if got := normalizeMonitor(s); got != MonitorEmbed {
			t.Errorf("normalizeMonitor(%q) = %q, want embed", s, got)
		}
	}
}

// ------------------------------------------------- command-line overrides

const overridableCfg = `
monitor_port: 8061
router_port: 8078
host: 127.0.0.1
engines:
  - name: e
    port: 8080
    command: /bin/true
    models: []
`

// README: "Overrides are not written back to the file". A save of any kind
// must leave the file's own ports alone, however many times it happens.
func TestOverridesNeverReachTheFile(t *testing.T) {
	p := writeCfg(t, overridableCfg)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	c.Overrides(8100, 8101, "0.0.0.0")
	if c.MonitorPort != 8100 || c.RouterPort != 8101 || c.Host != "0.0.0.0" {
		t.Fatalf("the overrides are not in force: %d %d %q", c.MonitorPort, c.RouterPort, c.Host)
	}

	// An edit that has nothing to do with the ports, saved twice: the second
	// save must not pick the flag values up from the first.
	for i := 0; i < 2; i++ {
		c.Engines[0].Models = append(c.Engines[0].Models, &Model{Name: fmt.Sprintf("m%d", i)})
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
	}
	onDisk, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.MonitorPort != 8061 || onDisk.RouterPort != 8078 || onDisk.Host != "127.0.0.1" {
		t.Errorf("the overrides leaked into %s: %d %d %q",
			p, onDisk.MonitorPort, onDisk.RouterPort, onDisk.Host)
	}
	// ... while the running configuration still listens where it was told to.
	if c.MonitorPort != 8100 || c.RouterPort != 8101 || c.Host != "0.0.0.0" {
		t.Errorf("Save changed the live configuration: %d %d %q", c.MonitorPort, c.RouterPort, c.Host)
	}
}

// Saving the settings form without touching the ports resubmits the values in
// force, which are the flag's. That must not be read as an edit.
func TestSettingsFormUntouchedKeepsTheOverride(t *testing.T) {
	p := writeCfg(t, overridableCfg)
	c, _ := LoadConfig(p)
	c.Overrides(8100, 0, "")

	if notes := c.SetListeners(c.MonitorPort, c.RouterPort, c.Host); len(notes) != 0 {
		t.Errorf("an untouched form reported changes: %v", notes)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := LoadConfig(p)
	if onDisk.MonitorPort != 8061 {
		t.Errorf("monitor_port in the file = %d, want the file's own 8061", onDisk.MonitorPort)
	}
	if c.MonitorPort != 8100 {
		t.Errorf("the override stopped applying: %d", c.MonitorPort)
	}
}

// Typing a different port into the page must take effect rather than be
// swallowed by the flag, and the page is told that the flag was dropped.
func TestSettingsPageEditBeatsTheOverride(t *testing.T) {
	p := writeCfg(t, overridableCfg)
	c, _ := LoadConfig(p)
	c.Overrides(8100, 0, "")

	notes := c.SetListeners(8200, c.RouterPort, c.Host)
	if len(notes) != 1 || !strings.Contains(notes[0], "-monitor-port 8100") {
		t.Fatalf("the dropped override was not reported: %v", notes)
	}
	if c.MonitorPort != 8200 {
		t.Errorf("the page edit did not take effect: %d", c.MonitorPort)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if onDisk, _ := LoadConfig(p); onDisk.MonitorPort != 8200 {
		t.Errorf("monitor_port in the file = %d, want the edited 8200", onDisk.MonitorPort)
	}
	if len(c.overridden()) != 0 {
		t.Errorf("the override survived the edit: %v", c.overridden())
	}
}

// A Config built in code - as the tests and NewManager do - has no file layer
// of its own, so normalize has to seed one instead of defaulting over it.
func TestNormalizeKeepsAHandBuiltPort(t *testing.T) {
	c := &Config{MonitorPort: 9001, RouterPort: 9002, Host: "::1"}
	c.normalize()
	if c.MonitorPort != 9001 || c.RouterPort != 9002 || c.Host != "::1" {
		t.Fatalf("normalize overwrote the ports: %d %d %q", c.MonitorPort, c.RouterPort, c.Host)
	}
	if c.file.MonitorPort != 9001 || c.file.RouterPort != 9002 || c.file.Host != "::1" {
		t.Errorf("the file layer was not seeded: %+v", c.file)
	}
}
