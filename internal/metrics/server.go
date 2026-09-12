package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server encapsulates the HTTP server exposing the /metrics endpoint.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	actualAddr string
}

// StartServer begins listening on addr and serves the /metrics and /healthz endpoints.
// If addr is empty or "none", server start is skipped.
func StartServer(addr string, m *Metrics) (*Server, error) {
	if addr == "" || addr == "none" {
		return nil, nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("starting metrics listener on %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	s := &Server{
		httpServer: srv,
		listener:   l,
		actualAddr: l.Addr().String(),
	}

	go func() {
		slog.Info("Prometheus metrics server listening", "addr", s.actualAddr)
		if serveErr := srv.Serve(l); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("Metrics server failed", "err", serveErr)
		}
	}()

	return s, nil
}

// Addr returns the actual listening address (useful when dynamic port ":0" is bound in tests).
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.actualAddr
}

// Stop gracefully shuts down the metrics HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	slog.InfoContext(ctx, "Shutting down Prometheus metrics server")
	return s.httpServer.Shutdown(ctx)
}
