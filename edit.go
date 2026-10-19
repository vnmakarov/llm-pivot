package main

import (
	"fmt"
	"sync"
)

// editMu serialises configuration mutations so that two browser tabs cannot
// interleave read-modify-write cycles.
var editMu sync.Mutex

// EditResult tells the monitor page what a configuration change implied.
type EditResult struct {
	Notes       []string `json:"notes"`
	PortsMoved  bool     `json:"ports_moved"`
	MonitorPort int      `json:"monitor_port"`
	RouterPort  int      `json:"router_port"`
}

// mutate applies fn to a copy of the configuration, validates the result,
// persists it and only then makes it current. The configuration is never
// modified in place, so goroutines holding the previous snapshot stay safe.
func (m *Manager) mutate(fn func(*Config) error) (*Config, *Config, error) {
	m.mu.RLock()
	old := m.cfg
	next := old.Clone()
	m.mu.RUnlock()

	if err := fn(next); err != nil {
		return nil, nil, err
	}
	next.normalize()
	if err := next.Validate(); err != nil {
		return nil, nil, err
	}
	if err := next.Save(); err != nil {
		return nil, nil, fmt.Errorf("cannot write %s: %w", next.Path(), err)
	}
	m.mu.Lock()
	m.cfg = next
	m.mu.Unlock()
	return old, next, nil
}

// stopIfStale terminates the running engine when the change invalidated its
// definition, and describes what it did.
func (m *Manager) stopIfStale(old, next *Config) []string {
	m.mu.RLock()
	in := m.inst
	var engine, model string
	if in != nil {
		engine, model = in.engine, in.model
	}
	m.mu.RUnlock()
	if in == nil {
		return nil
	}

	oldEng, oldMdl := old.FindModel(model)
	newEng, newMdl := next.FindModel(model)

	var reason string
	switch {
	case newEng == nil:
		reason = fmt.Sprintf("model %q no longer exists", model)
	case newEng.Name != engine:
		reason = fmt.Sprintf("model %q moved to engine %q", model, newEng.Name)
	case oldEng == nil:
		reason = "configuration changed"
	case newEng.Command != oldEng.Command || newEng.Port != oldEng.Port ||
		newEng.MonitorMode() != oldEng.MonitorMode() || newEng.MonitorURL != oldEng.MonitorURL ||
		newEng.HealthPath != oldEng.HealthPath || newEng.PortFlag != oldEng.PortFlag ||
		newEng.WorkDir != oldEng.WorkDir || !sameStrings(newEng.Env, oldEng.Env):
		reason = fmt.Sprintf("engine %q was reconfigured", engine)
	case oldMdl != nil && newMdl != nil && newMdl.Options != oldMdl.Options:
		reason = fmt.Sprintf("options of model %q changed", model)
	}
	if reason == "" {
		return nil
	}

	m.opMu.Lock()
	m.stopLocked(reason)
	m.opMu.Unlock()
	return []string{fmt.Sprintf("Stopped the running engine: %s. Start it again when you are ready.", reason)}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Reload re-reads the configuration file from disk, which is what SIGHUP
// (systemctl --user reload engine-router) does. A file that does not
// validate is rejected and the running configuration is kept.
func (m *Manager) Reload() (*EditResult, error) {
	editMu.Lock()
	defer editMu.Unlock()

	m.mu.RLock()
	old := m.cfg
	m.mu.RUnlock()

	next, err := LoadConfig(old.Path())
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cfg = next
	m.mu.Unlock()

	res := &EditResult{
		MonitorPort: next.MonitorPort,
		RouterPort:  next.RouterPort,
		PortsMoved: old.MonitorPort != next.MonitorPort ||
			old.RouterPort != next.RouterPort || old.Host != next.Host,
	}
	res.Notes = m.stopIfStale(old, next)
	return res, nil
}

// SettingsInput is the payload of the global settings form.
type SettingsInput struct {
	MonitorPort    int    `json:"monitor_port"`
	RouterPort     int    `json:"router_port"`
	Host           string `json:"host"`
	StartupTimeout int    `json:"startup_timeout_seconds"`
	StopTimeout    int    `json:"stop_timeout_seconds"`
	LogLines       int    `json:"log_lines"`
}

// UpdateSettings changes the global options, including the two ports.
func (m *Manager) UpdateSettings(in SettingsInput) (*EditResult, error) {
	editMu.Lock()
	defer editMu.Unlock()

	old, next, err := m.mutate(func(c *Config) error {
		c.MonitorPort = in.MonitorPort
		c.RouterPort = in.RouterPort
		c.Host = in.Host
		c.StartupTimeout = in.StartupTimeout
		c.StopTimeout = in.StopTimeout
		if in.LogLines > 0 {
			c.LogLines = in.LogLines
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &EditResult{
		MonitorPort: next.MonitorPort,
		RouterPort:  next.RouterPort,
		PortsMoved:  old.MonitorPort != next.MonitorPort || old.RouterPort != next.RouterPort || old.Host != next.Host,
	}
	res.Notes = append(res.Notes, m.stopIfStale(old, next)...)
	return res, nil
}

// EngineInput is the payload of the engine form.
type EngineInput struct {
	Original       string   `json:"original"` // empty when creating
	Name           string   `json:"name"`
	Port           int      `json:"port"`
	Command        string   `json:"command"`
	Monitor        string   `json:"monitor"`
	MonitorURL     string   `json:"monitor_url"`
	MonitorEvery   int      `json:"monitor_interval_seconds"`
	HealthPath     string   `json:"health_path"`
	PortFlag       string   `json:"port_flag"`
	StartupTimeout int      `json:"startup_timeout_seconds"`
	WorkDir        string   `json:"work_dir"`
	Env            []string `json:"env"`
}

// SaveEngine creates or updates an engine definition.
func (m *Manager) SaveEngine(in EngineInput) (*EditResult, error) {
	editMu.Lock()
	defer editMu.Unlock()

	old, next, err := m.mutate(func(c *Config) error {
		apply := func(e *Engine) {
			e.Name = in.Name
			e.Port = in.Port
			e.Command = in.Command
			e.Monitor = in.Monitor
			e.MonitorURL = in.MonitorURL
			e.MonitorEvery = in.MonitorEvery
			e.HealthPath = in.HealthPath
			e.PortFlag = in.PortFlag
			e.StartupTimeout = in.StartupTimeout
			e.WorkDir = in.WorkDir
			e.Env = in.Env
		}
		if in.Original == "" {
			if c.Engine(in.Name) != nil {
				return fmt.Errorf("an engine named %q already exists", in.Name)
			}
			e := &Engine{}
			apply(e)
			c.Engines = append(c.Engines, e)
			return nil
		}
		e := c.Engine(in.Original)
		if e == nil {
			return fmt.Errorf("unknown engine %q", in.Original)
		}
		if in.Name != in.Original && c.Engine(in.Name) != nil {
			return fmt.Errorf("an engine named %q already exists", in.Name)
		}
		apply(e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &EditResult{MonitorPort: next.MonitorPort, RouterPort: next.RouterPort}
	res.Notes = append(res.Notes, m.stopIfStale(old, next)...)
	return res, nil
}

// DeleteEngine removes an engine together with all of its models.
func (m *Manager) DeleteEngine(name string) (*EditResult, error) {
	editMu.Lock()
	defer editMu.Unlock()

	old, next, err := m.mutate(func(c *Config) error {
		for i, e := range c.Engines {
			if e.Name == name {
				c.Engines = append(c.Engines[:i], c.Engines[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("unknown engine %q", name)
	})
	if err != nil {
		return nil, err
	}
	res := &EditResult{MonitorPort: next.MonitorPort, RouterPort: next.RouterPort}
	res.Notes = append(res.Notes, m.stopIfStale(old, next)...)
	return res, nil
}

// ModelInput is the payload of the model form.
type ModelInput struct {
	Engine   string `json:"engine"`
	Original string `json:"original"` // empty when creating
	Name     string `json:"name"`
	Options  string `json:"options"`
}

// SaveModel creates or updates a model. Model names are unique across every
// engine, which Validate enforces for us.
func (m *Manager) SaveModel(in ModelInput) (*EditResult, error) {
	editMu.Lock()
	defer editMu.Unlock()

	old, next, err := m.mutate(func(c *Config) error {
		e := c.Engine(in.Engine)
		if e == nil {
			return fmt.Errorf("unknown engine %q", in.Engine)
		}
		if in.Original == "" {
			if owner, _ := c.FindModel(in.Name); owner != nil {
				return fmt.Errorf("a model named %q already exists in engine %q", in.Name, owner.Name)
			}
			e.Models = append(e.Models, &Model{Name: in.Name, Options: in.Options})
			return nil
		}
		mdl := e.Model(in.Original)
		if mdl == nil {
			return fmt.Errorf("engine %q has no model %q", in.Engine, in.Original)
		}
		if in.Name != in.Original {
			if owner, _ := c.FindModel(in.Name); owner != nil {
				return fmt.Errorf("a model named %q already exists in engine %q", in.Name, owner.Name)
			}
		}
		mdl.Name = in.Name
		mdl.Options = in.Options
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &EditResult{MonitorPort: next.MonitorPort, RouterPort: next.RouterPort}
	res.Notes = append(res.Notes, m.stopIfStale(old, next)...)
	return res, nil
}

// DeleteModel removes a single model from an engine.
func (m *Manager) DeleteModel(engine, model string) (*EditResult, error) {
	editMu.Lock()
	defer editMu.Unlock()

	old, next, err := m.mutate(func(c *Config) error {
		e := c.Engine(engine)
		if e == nil {
			return fmt.Errorf("unknown engine %q", engine)
		}
		for i, mdl := range e.Models {
			if mdl.Name == model {
				e.Models = append(e.Models[:i], e.Models[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("engine %q has no model %q", engine, model)
	})
	if err != nil {
		return nil, err
	}
	res := &EditResult{MonitorPort: next.MonitorPort, RouterPort: next.RouterPort}
	res.Notes = append(res.Notes, m.stopIfStale(old, next)...)
	return res, nil
}
