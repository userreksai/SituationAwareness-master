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

	"github.com/userreksai/SituationAwareness-master/internal/master"
)

func main() {
	logger := log.New(os.Stdout, "master ", log.LstdFlags|log.LUTC)
	cfg, err := master.LoadConfig()
	if err != nil {
		logger.Fatalf("invalid configuration: %v", err)
	}
	if cfg.SharedToken == "" {
		logger.Printf("WARNING: AGENT_SHARED_TOKEN is empty; Agent calls are not authenticated")
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           master.NewHandler(cfg, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.TaskMaxTimeout + 10*time.Second,
		WriteTimeout:      cfg.TaskMaxTimeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s, registry=%s, agent_port=%d", cfg.ListenAddr, cfg.RegistryURL, cfg.AgentPort)
		errCh <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-stop:
		logger.Printf("received %s, shutting down", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server stopped: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Printf("graceful shutdown failed: %v", err)
	}
}
