package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Printf(`Antigravity Proxy

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
		return
	}
	cfg, path, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			if err := oauth.Login(cfg, path); err != nil {
				log.Fatal(err)
			}
			return
		case "serve":
		default:
			log.Fatalf("unknown command %q (use --help)", os.Args[1])
		}
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		log.Printf("No Google credential configured; run `antigravity-proxy login` or set ANTIGRAVITY_ACCESS_TOKEN")
	}

	if err := serve(cfg, path); err != nil {
		log.Fatal(err)
	}
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
	log.Printf("Antigravity Proxy listening on http://%s", address)
	log.Printf("Quota polling every %d seconds; history: %s", cfg.QuotaPollIntervalSeconds, cfg.QuotaHistoryPath)
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
