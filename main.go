// Command llm-pivot starts, stops and monitors local LLM engines
// (llama.cpp, strata, ninfer, ...) and routes OpenAI or Anthropic requests to
// whichever engine currently serves the requested model.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is reported on the monitor page and by -version.
const Version = "1.0.0"

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	log.SetPrefix("llm-pivot: ")

	var (
		configPath  = flag.String("config", defaultConfigPath(), "path of the YAML configuration file")
		monitorPort = flag.Int("monitor-port", 0, "override monitor_port from the configuration")
		routerPort  = flag.Int("router-port", 0, "override router_port from the configuration")
		host        = flag.String("host", "", "override the bind address from the configuration")
		initOnly    = flag.Bool("init", false, "write a default configuration file and exit")
		checkOnly   = flag.Bool("check", false, "validate the configuration file and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("llm-pivot", Version)
		return
	}

	if *initOnly {
		if _, err := os.Stat(*configPath); err == nil {
			die("%s already exists; refusing to overwrite it", *configPath)
		}
		if err := WriteDefaultConfig(*configPath); err != nil {
			die("cannot write %s: %v", *configPath, err)
		}
		fmt.Printf("wrote default configuration to %s\n", *configPath)
		return
	}

	// A missing configuration is not an error on first start: seed it.
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := WriteDefaultConfig(*configPath); err != nil {
			die("cannot create %s: %v", *configPath, err)
		}
		log.Printf("created default configuration %s", *configPath)
	}

	// Any problem here - unknown option, duplicated model name, bad port -
	// stops the program with the reason.
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		die("%v", err)
	}
	// LoadConfig has already validated the file on its own; validate again
	// with the flags layered on, because an override can collide with a port
	// the file is happy with.
	cfg.Overrides(*monitorPort, *routerPort, *host)
	if err := cfg.Validate(); err != nil {
		die("%v", err)
	}

	if *checkOnly {
		fmt.Printf("%s: configuration is valid (%d engine(s), %d model(s))\n",
			*configPath, len(cfg.Engines), countModels(cfg))
		return
	}

	mgr := NewManager(cfg)
	monitorSrv := NewReloadable("monitor", nil)
	routerSrv := NewReloadable("router", NewRouter(mgr))

	// The API can move both listeners when the ports are edited on the page.
	rebind := func(h string, mp, rp int) {
		if mh, mport := monitorSrv.Addr(); mh != h || mport != mp {
			if err := monitorSrv.Bind(h, mp); err != nil {
				log.Printf("cannot move the monitor page to %s: %v",
					net.JoinHostPort(h, strconv.Itoa(mp)), err)
				mgr.Log().System(fmt.Sprintf("✗ cannot move the monitor page to port %d: %v", mp, err))
			}
		}
		if rh, rport := routerSrv.Addr(); rh != h || rport != rp {
			if err := routerSrv.Bind(h, rp); err != nil {
				log.Printf("cannot move the router to %s: %v",
					net.JoinHostPort(h, strconv.Itoa(rp)), err)
				mgr.Log().System(fmt.Sprintf("✗ cannot move the router to port %d: %v", rp, err))
			}
		}
	}
	monitorSrv.handler = NewAPI(mgr, rebind)

	if err := monitorSrv.Bind(cfg.Host, cfg.MonitorPort); err != nil {
		die("cannot listen on the monitor port: %v", err)
	}
	if err := routerSrv.Bind(cfg.Host, cfg.RouterPort); err != nil {
		monitorSrv.Close()
		die("cannot listen on the router port: %v", err)
	}

	mgr.Log().System(fmt.Sprintf("llm-pivot %s started; configuration %s", Version, cfg.Path()))
	mgr.Log().System(fmt.Sprintf("monitor page on port %d, API router on port %d",
		cfg.MonitorPort, cfg.RouterPort))
	// The page would otherwise show a value that is nowhere in the file.
	if over := cfg.overridden(); len(over) > 0 {
		mgr.Log().System(fmt.Sprintf("%s overridden on the command line for this run; %s keeps its own value",
			strings.Join(over, " and "), cfg.Path()))
	}
	log.Printf("configuration %s (%d engine(s), %d model(s))",
		cfg.Path(), len(cfg.Engines), countModels(cfg))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	// The sweep granularity is added to inactive_stop_timeout_seconds, so an
	// idle engine is stopped somewhere within this long of its deadline.
	idleSweep := time.NewTicker(30 * time.Second)
	defer idleSweep.Stop()

signals:
	for {
		select {
		case s := <-sig:
			if s != syscall.SIGHUP {
				log.Printf("received %s, shutting down", s)
				break signals
			}
			// systemctl --user reload llm-pivot
			res, err := mgr.Reload()
			if err != nil {
				log.Printf("reload refused: %v", err)
				mgr.Log().System("✗ reload refused, keeping the previous configuration: " + err.Error())
				continue
			}
			log.Printf("configuration reloaded from %s", mgr.Config().Path())
			mgr.Log().System("configuration reloaded from " + mgr.Config().Path())
			for _, n := range res.Notes {
				mgr.Log().System(n)
			}
			if res.PortsMoved {
				c := mgr.Config()
				rebind(c.Host, c.MonitorPort, c.RouterPort)
			}
		case <-idleSweep.C:
			mgr.CheckInactivity()
		}
	}

	monitorSrv.Close()
	routerSrv.Close()
	mgr.Shutdown() // never leave an engine behind
	log.Printf("stopped")
}

func countModels(c *Config) int {
	n := 0
	for _, e := range c.Engines {
		n += len(e.Models)
	}
	return n
}

// defaultConfigPath honours $LLM_PIVOT_CONFIG, then $XDG_CONFIG_HOME.
func defaultConfigPath() string {
	if p := os.Getenv("LLM_PIVOT_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "llm-pivot.yaml"
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "llm-pivot", "config.yaml")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llm-pivot: %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}

func usage() {
	fmt.Fprintf(os.Stderr, `llm-pivot %s - start, stop and monitor local LLM engines.

Usage:
  llm-pivot [flags]

The monitor page (default port 8061) manages engines and models. The router
port (default 8078) detects OpenAI and Anthropic requests automatically and
forwards them to the engine serving the requested model, starting it if necessary.

Flags:
`, Version)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nDefault configuration file: %s\n", defaultConfigPath())
}
