// Command webui is the htmx web interface for managing the library. It is for
// internal use: it does not publish the podcast feeds (syncr does). It shares
// the config file and the data directory with the sync service (cmd/syncr),
// reading and writing those directly, and calls syncr's small control API only
// to apply changes, trigger pulls, read live status and look up sources.
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

	"podsync/fs"
	"podsync/rest"
	"podsync/toml"
)

var version = "dev"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfgPath := flag.String("config", envOr("PODSYNC_CONFIG", "config.toml"), "path to the config file shared with syncr (env PODSYNC_CONFIG)")
	listen := flag.String("listen", envOr("WEBUI_LISTEN", ":8090"), "address to serve the web UI on (env WEBUI_LISTEN)")
	syncr := flag.String("syncr", envOr("SYNCR_URL", "http://127.0.0.1:8081"), "base URL of the syncr API (env SYNCR_URL)")
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
		if err := run(*cfgPath, *listen, *syncr); err != nil {
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

func run(cfgPath, listen, syncr string) error {
	u, err := url.Parse(syncr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("-syncr must be an absolute http(s) URL, got %q", syncr)
	}
	cfg, err := toml.Open(cfgPath)
	if err != nil {
		return err
	}
	dataDir := cfg.Get().Server.DataDir
	lib := fs.New(dataDir)
	if err := lib.Load(); err != nil {
		return fmt.Errorf("load library from %s: %w", dataDir, err)
	}
	ui, err := newServer(options{Config: cfg, Library: lib, Syncer: rest.NewClient(syncr, nil)})
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
		// No write timeout: media downloads to slow podcast clients can take long.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("webui %s listening on %s (config %s, data %s, syncr at %s)", version, listen, cfgPath, dataDir, syncr)

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
