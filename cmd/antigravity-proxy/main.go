package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"antigravity-proxy/internal/proxy"
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
			fmt.Println("Antigravity Proxy\n\nUsage:\n  antigravity-proxy [serve]\n  antigravity-proxy login\n\nEnvironment:\n  HOST, PORT, API_KEY, ANTIGRAVITY_ACCESS_TOKEN, ANTIGRAVITY_REFRESH_TOKEN,\n  ANTIGRAVITY_OAUTH_CLIENT_ID, ANTIGRAVITY_OAUTH_CLIENT_SECRET,\n  ANTIGRAVITY_PROJECT_ID, ANTIGRAVITY_DAILY_ENDPOINT, ANTIGRAVITY_PROD_ENDPOINT")
			return
		case "serve":
		default:
			log.Fatalf("unknown command %q (use --help)", os.Args[1])
		}
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		log.Printf("No Google credential configured; run `antigravity-proxy login` or set ANTIGRAVITY_ACCESS_TOKEN")
	}

	server := proxy.New(cfg)
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		// Keep write deadlines disabled because upstream requests can take several minutes.
	}
	log.Printf("Antigravity Proxy listening on http://%s", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	log.Fatal(httpServer.ListenAndServe())
}
