// Bifrost is a self-hosted webhook bridge. See README.md.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bifrost/internal/config"
	"bifrost/internal/server"
)

func main() {
	configPath := os.Getenv("CONFIG")
	if configPath == "" {
		configPath = "config.yaml"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = cfg.Addr
	}

	srv, err := server.NewServer(cfg)
	if err != nil {
		log.Fatalf("server error: %v", err)
	}

	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      srv.Routes(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Run the server in a goroutine so we can listen for shutdown signals.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("bifrost listening on %s (config: %s, flows: %d)", addr, configPath, len(cfg.Flows))
		errCh <- httpSrv.ListenAndServe()
	}()

	// Block until we receive an interrupt signal or the server errors out.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
	}

	// Give in-flight requests up to 10 seconds to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("server stopped")
}
