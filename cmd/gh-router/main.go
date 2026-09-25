package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/takonomura/gh-router/internal/ghrouter"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "gh-router: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 0 {
		switch args[0] {
		case "serve":
			return runServe(args[1:])
		case "exec":
			return runExec(args[1:])
		case ghrouter.SidecarCommand:
			return runSidecar(args[1:])
		}
	}
	// Keep the original `gh-router -config ...` invocation working.
	return runServe(args)
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("gh-router serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "config.json", "path to the JSON configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve: unexpected argument %q", flags.Arg(0))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := ghrouter.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	proxy, err := ghrouter.NewProxy(cfg, logger)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(os.Stderr, "gh-router http: ", log.LstdFlags),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("proxy started", "listen", cfg.Server.Listen)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("proxy stopping")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func runExec(args []string) error {
	flags := flag.NewFlagSet("gh-router exec", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "config.json", "path to the JSON configuration file")
	hint := flags.String("hint", "", "credential routing hint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("exec: command is required")
	}
	return ghrouter.ExecCommand(*configPath, *hint, flags.Args())
}

func runSidecar(args []string) error {
	flags := flag.NewFlagSet("gh-router sidecar", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to the JSON configuration file")
	parentPID := flags.Int("parent-pid", 0, "executor process ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *configPath == "" || *parentPID <= 0 {
		return errors.New("invalid internal sidecar invocation")
	}
	ready := os.NewFile(uintptr(3), "gh-router-ready")
	if ready == nil {
		return errors.New("sidecar readiness pipe is unavailable")
	}
	defer ready.Close()
	authentication := os.NewFile(uintptr(4), "gh-router-authentication")
	if authentication == nil {
		return errors.New("executor authentication pipe is unavailable")
	}
	defer authentication.Close()
	return ghrouter.RunSidecar(*configPath, *parentPID, ready, authentication)
}
