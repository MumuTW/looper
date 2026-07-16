package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

type Server struct {
	config   config.Config
	handler  http.Handler
	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	done     chan struct{}
	errors   chan error
}

func NewServer(cfg config.Config, handler http.Handler) *Server {
	return &Server{config: cfg, handler: handler}
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.server != nil {
		return nil
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(s.config.Server.Host, strconv.Itoa(s.config.Server.Port)))
	if err != nil {
		return fmt.Errorf("listen on %s:%d: %w", s.config.Server.Host, s.config.Server.Port, err)
	}

	done := make(chan struct{})
	serveErrors := make(chan error, 1)
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	s.listener = listener
	s.server = server
	s.done = done
	s.errors = serveErrors

	go func() {
		defer close(done)
		defer close(serveErrors)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrors <- fmt.Errorf("serve API: %w", err)
		}
	}()

	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	done := s.done
	s.server = nil
	s.listener = nil
	s.done = nil
	s.errors = nil
	s.mu.Unlock()

	if server == nil {
		return nil
	}

	err := server.Shutdown(ctx)
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
		}
	}
	return err
}

// Errors reports failures that happen after the listener has started. The
// channel closes on an orderly Stop.
func (s *Server) Errors() <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener == nil {
		return nil
	}

	return s.listener.Addr()
}
