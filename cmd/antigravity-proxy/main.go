package main

import (
	"antigravity-proxy/internal/buildinfo"
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/proxy"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:], os.Stdout); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) > 0 && args[0] == "version" {
		if len(args) != 1 {
			return errors.New("usage: antigravity-proxy version")
		}
		_, err := fmt.Fprintln(output, buildinfo.Current())
		return err
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		variables := config.EnvironmentVariables()
		defaults := map[string]any{
			"ANTIGRAVITY_CLIENT_VERSION":              config.DefaultClientVersion,
			"ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS":  config.DefaultMaxConcurrentGenerations,
			"ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS": config.DefaultQuotaPollIntervalSeconds,
			"ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES":   config.DefaultQuotaHistoryMaxSamples,
		}
		for i, name := range variables {
			if value, ok := defaults[name]; ok {
				variables[i] += fmt.Sprintf(" (default %v)", value)
			}
		}
		_, err := fmt.Fprintf(output, "Antigravity Proxy\n\nUsage:\n  antigravity-proxy [serve]\n  antigravity-proxy login [--account ID] [--replace] [--issue-key LABEL]\n  antigravity-proxy accounts <list|enable|disable|remove> [ID]\n  antigravity-proxy keys <list|create|revoke> <ACCOUNT_ID> [KEY_ID] [--label LABEL]\n  antigravity-proxy admin-key rotate\n\nEnvironment:\n  %s\n", strings.Join(variables, "\n  "))
		return err
	}
	cfg, path, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "login":
			return runLoginCommand(args[1:], cfg, path, output)
		case "accounts":
			return runAccountsCommand(args[1:], cfg, path, output)
		case "keys":
			return runKeysCommand(args[1:], cfg, path, output)
		case "admin-key":
			return runAdminKeyCommand(args[1:], path, output)
		case "serve":
		default:
			return fmt.Errorf("unknown command %q (use --help)", args[0])
		}
	}
	if !cfg.HasCredentials() {
		slog.Info("No Google credential configured; run `antigravity-proxy login` or set ANTIGRAVITY_ACCESS_TOKEN")
	}
	return serve(cfg, path)
}

// serve owns HTTP and quota-polling lifecycles so every exit closes history.
func serve(cfg config.Config, configPath string) (err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = listener.Close() }()
	server, err := proxy.NewRuntime(ctx, cfg, configPath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close account runtime: %w", closeErr))
		}
	}()
	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		// Keep write deadlines disabled because upstream requests can take several minutes.
	}
	info := buildinfo.Current()
	slog.Info("Antigravity Proxy listening", "address", "http://"+address, "version", info.Version, "commit", info.Commit, "date", info.Date)
	slog.Info("Quota polling configured", "interval_seconds", cfg.QuotaPollIntervalSeconds, "history", cfg.QuotaHistoryPath)
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	select {
	case serveErr := <-served:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if shutdownErr := httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
			return errors.Join(shutdownErr, httpServer.Close())
		}
		return nil
	}
}
