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

	"github.com/MumuTW/looper/internal/bootstrap"
	"github.com/MumuTW/looper/internal/config"
)

type Server struct {
	config   config.Config
	handler  http.Handler
	logger   bootstrap.Logger
	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	done     chan struct{}
}

func NewServer(cfg config.Config, handler http.Handler, logger bootstrap.Logger) *Server {
	return &Server{config: cfg, handler: handler, logger: logger}
}

// newHTTPServer builds the daemon HTTP server with its request-read bounds:
// headers within 30s, the whole request (including body) within 2 minutes, so
// a slow or stalled body cannot hold a goroutine indefinitely. Responses (SSE
// streams) are unaffected — these bound reads, not writes.
func (s *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       2 * time.Minute,
	}
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
	server := s.newHTTPServer()

	s.listener = listener
	s.server = server
	s.done = done

	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Serve only returns here on an unexpected accept/serve failure;
			// without this log the API surface disappears silently while the
			// daemon stays alive.
			if s.logger != nil {
				s.logger.Error("api server stopped unexpectedly", map[string]any{
					"error": err.Error(),
					"addr":  listener.Addr().String(),
				})
			}
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
	s.mu.Unlock()

	if server == nil {
		return nil
	}

	err := server.Shutdown(ctx)
	if done != nil {
		<-done
	}
	return err
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener == nil {
		return nil
	}

	return s.listener.Addr()
}
