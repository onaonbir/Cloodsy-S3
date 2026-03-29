package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func RunServer(handler *Handler, listen string, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/admin/", handler)

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Error("admin server listen failed", "addr", listen, "error", err)
		return nil
	}

	go func() {
		logger.Info(fmt.Sprintf("Admin API listening on %s", listen))
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("admin server error", "error", err)
		}
	}()

	return srv
}

func StopServer(srv *http.Server, logger *slog.Logger) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("admin server shutdown error", "error", err)
	}
}
