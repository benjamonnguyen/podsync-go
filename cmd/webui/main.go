// Command webui is the htmx web interface. It holds no state of its own: it
// talks to the sync service (cmd/syncr) over its REST API and proxies the RSS
// feeds and media files from it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"podsync/rest"
	"podsync/webui"
)

var version = "dev"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", envOr("WEBUI_LISTEN", ":8080"), "address to serve the web UI on (env WEBUI_LISTEN)")
	syncr := flag.String("syncr", envOr("SYNCR_URL", "http://127.0.0.1:8081"), "base URL of the syncr service (env SYNCR_URL)")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the running web UI (for container health checks) and exit 0 if healthy")
	flag.Parse()
	switch {
	case *showVersion:
		fmt.Println("webui", version)
	case *healthcheck:
		if err := probe(*listen); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
	default:
		if err := run(*listen, *syncr); err != nil {
			log.Fatalf("error: %v", err)
		}
	}
}

func probe(listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func run(listen, syncr string) error {
	upstream, err := url.Parse(syncr)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return fmt.Errorf("-syncr must be an absolute http(s) URL, got %q", syncr)
	}
	ui, err := webui.New(rest.NewClient(syncr, nil), upstream)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              listen,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: proxied media streams to slow podcast clients can take long.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("webui %s listening on %s (syncr at %s)", version, listen, syncr)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Print("shutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
	return nil
}
