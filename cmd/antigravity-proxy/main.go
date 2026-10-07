package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	"antigravity-proxy/internal/status"
)

func main() {
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
		case "help", "-h", "--help":
			fmt.Println("Antigravity Proxy\n\nUsage:\n  antigravity-proxy [serve]\n  antigravity-proxy login\n\nEnvironment:\n  HOST, PORT, API_KEY, ANTIGRAVITY_ACCESS_TOKEN, ANTIGRAVITY_REFRESH_TOKEN,\n  ANTIGRAVITY_OAUTH_CLIENT_ID, ANTIGRAVITY_OAUTH_CLIENT_SECRET,\n  ANTIGRAVITY_PROJECT_ID, ANTIGRAVITY_DAILY_ENDPOINT, ANTIGRAVITY_PROD_ENDPOINT,\n  ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS (default 300), ANTIGRAVITY_QUOTA_HISTORY_PATH")
			return
		case "serve":
		default:
			log.Fatalf("unknown command %q (use --help)", os.Args[1])
		}
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		log.Printf("No Google credential configured; run `antigravity-proxy login` or set ANTIGRAVITY_ACCESS_TOKEN")
	}

	if err := serve(cfg); err != nil {
		log.Fatal(err)
	}
}

// serve owns HTTP and quota-polling lifecycles so every exit closes history.
func serve(cfg config.Config) (err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	server := proxy.New(cfg)
	quotaStatus := status.NewService(cfg, server.QuotaFetcher().Fetch)
	server.StatusHandler = quotaStatus
	if err := quotaStatus.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if closeErr := quotaStatus.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close quota history: %w", closeErr))
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
