package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Reloadable is an http server whose listening address can be changed while
// the program runs, which is what makes the two ports editable from the page.
type Reloadable struct {
	name    string
	handler http.Handler

	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	host string
	port int
}

func NewReloadable(name string, h http.Handler) *Reloadable {
	return &Reloadable{name: name, handler: h}
}

// Bind starts serving on host:port. The previous listener, if any, is shut
// down only after the new one has been established, so a failed rebind
// leaves the old address working.
func (s *Reloadable) Bind(host string, port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
		// No write timeout: engine responses are streamed.
	}

	s.mu.Lock()
	oldSrv := s.srv
	s.srv, s.ln, s.host, s.port = srv, ln, host, port
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("%s: server stopped: %v", s.name, err)
		}
	}()
	log.Printf("%s: listening on http://%s", s.name, net.JoinHostPort(host, strconv.Itoa(port)))

	if oldSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := oldSrv.Shutdown(ctx); err != nil {
			_ = oldSrv.Close()
		}
	}
	return nil
}

// Addr is the address currently served.
func (s *Reloadable) Addr() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host, s.port
}

// Close stops serving.
func (s *Reloadable) Close() {
	s.mu.Lock()
	srv := s.srv
	s.srv, s.ln = nil, nil
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
	}
}
