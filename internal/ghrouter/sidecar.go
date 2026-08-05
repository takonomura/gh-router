//go:build linux || darwin

package ghrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const parentPollInterval = 250 * time.Millisecond

func RunSidecar(configPath string, parentPID int, ready *os.File) error {
	readyReported := false
	if err := runSidecar(configPath, parentPID, ready, &readyReported); err != nil {
		if readyReported {
			return err
		}
		if writeErr := json.NewEncoder(ready).Encode(sidecarReady{Error: err.Error()}); writeErr != nil {
			return fmt.Errorf("sidecar startup failed: %v; report error: %w", err, writeErr)
		}
		return nil
	}
	return nil
}

func runSidecar(configPath string, parentPID int, ready *os.File, readyReported *bool) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	proxy, err := NewProxy(cfg, logger)
	if err != nil {
		return err
	}

	directory, err := os.MkdirTemp("", "gh-router-")
	if err != nil {
		return fmt.Errorf("create temporary CA directory: %w", err)
	}
	defer os.RemoveAll(directory)
	caPath := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(caPath, proxy.ca.certificatePEM(), 0o600); err != nil {
		return fmt.Errorf("write public CA certificate: %w", err)
	}

	listener, err := net.Listen("tcp", temporaryListenAddress)
	if err != nil {
		return fmt.Errorf("listen on temporary proxy address: %w", err)
	}
	server := &http.Server{
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(os.Stderr, "gh-router sidecar http: ", log.LstdFlags),
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()

	if os.Getppid() != parentPID {
		listener.Close()
		return errors.New("executor exited during sidecar startup")
	}
	readyState := sidecarReady{
		ProxyURL:      "http://" + listener.Addr().String(),
		CACertificate: caPath,
	}
	if err := json.NewEncoder(ready).Encode(readyState); err != nil {
		listener.Close()
		return fmt.Errorf("report sidecar readiness: %w", err)
	}
	*readyReported = true
	_ = ready.Close()

	parentGone := make(chan struct{}, 1)
	monitorDone := make(chan struct{})
	defer close(monitorDone)
	go monitorParent(parentPID, parentGone, monitorDone)

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve temporary proxy: %w", err)
	case <-parentGone:
	case <-signalCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shutdown temporary proxy: %w", err)
	}
	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve temporary proxy: %w", err)
	}
	return nil
}

func monitorParent(parentPID int, gone chan<- struct{}, done <-chan struct{}) {
	ticker := time.NewTicker(parentPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if os.Getppid() != parentPID {
				gone <- struct{}{}
				return
			}
		case <-done:
			return
		}
	}
}
