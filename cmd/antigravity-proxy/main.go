package main

import (
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
	"syscall"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"antigravity-proxy/internal/proxy"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:], os.Stdout); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintf(output, `Antigravity Proxy

Usage:
  antigravity-proxy [serve]
  antigravity-proxy login

Environment:
  HOST, PORT, API_KEY, ANTIGRAVITY_ACCESS_TOKEN, ANTIGRAVITY_REFRESH_TOKEN,
  ANTIGRAVITY_OAUTH_CLIENT_ID, ANTIGRAVITY_OAUTH_CLIENT_SECRET,
  ANTIGRAVITY_PROJECT_ID, ANTIGRAVITY_DAILY_ENDPOINT, ANTIGRAVITY_PROD_ENDPOINT,
  ANTIGRAVITY_CLIENT_VERSION (default %s), OAUTH_CALLBACK_PORT,
  ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS (default %d),
  ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS (default %d),
  ANTIGRAVITY_QUOTA_HISTORY_PATH,
  ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES (default %d)
`, config.DefaultClientVersion, config.DefaultMaxConcurrentGenerations,
			config.DefaultQuotaPollIntervalSeconds, config.DefaultQuotaHistoryMaxSamples)
		return nil
	}
	cfg, path, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "login":
			return oauth.Login(cfg, path)
		case "serve":
		default:
			return fmt.Errorf("unknown command %q (use --help)", args[0])
		}
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
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
	defer listener.Close()
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
	slog.Info("Antigravity Proxy listening", "address", "http://"+address)
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
