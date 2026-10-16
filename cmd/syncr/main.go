// Command syncr is the sync service: it fetches episodes from feed sources
// (YouTube via yt-dlp) into the library on disk and publishes the podcast
// feeds and media files. It listens on two separate addresses: a public one
// for the feeds and media (server.listen_addr), and a private control API
// (server.api_addr) that the web UI (cmd/webui) uses to apply changes, trigger
// pulls, read live status and look up sources. The web UI shares the config
// file and data directory with syncr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"podsync"
	"podsync/fs"
	"podsync/publish"
	"podsync/rest"
	"podsync/status"
	"podsync/syncer"
	"podsync/toml"
	"podsync/youtube"
)

var version = "dev"

// apiAddr is where the control API listens. SYNCR_API_ADDR overrides the config
// file so a container can bind it to all interfaces (the web UI lives in another
// container) while the file keeps the safe loopback default.
func apiAddr(c podsync.Config) string {
	if v := os.Getenv("SYNCR_API_ADDR"); v != "" {
		return v
	}
	return c.Server.APIAddr
}

func main() {
	defPath := "config.toml"
	if v := os.Getenv("PODSYNC_CONFIG"); v != "" {
		defPath = v
	}
	cfgPath := flag.String("config", defPath, "path to the TOML config file (created with defaults if missing; env PODSYNC_CONFIG)")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the running service (for container health checks) and exit 0 if healthy")
	flag.Parse()
	switch {
	case *showVersion:
		fmt.Println("syncr", version)
	case *healthcheck:
		if err := probe(*cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
	default:
		if err := run(*cfgPath); err != nil {
			log.Fatalf("error: %v", err)
		}
	}
}

// probe checks that both listeners named in the config answer /healthz.
func probe(cfgPath string) error {
	cfg, err := toml.Open(cfgPath)
	if err != nil {
		return err
	}
	c := cfg.Get()
	client := http.Client{Timeout: 4 * time.Second}
	for _, addr := range []string{c.Server.ListenAddr, apiAddr(c)} {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: status %s", addr, resp.Status)
		}
	}
	return nil
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: media downloads to slow podcast clients can take long.
	}
}

func run(cfgPath string) error {
	cfg, err := toml.Open(cfgPath)
	if err != nil {
		return err
	}
	c := cfg.Get()

	lib := fs.New(c.Server.DataDir)
	if err := lib.Load(); err != nil {
		return fmt.Errorf("load library from %s: %w", c.Server.DataDir, err)
	}
	if err := lib.CleanTemp(); err != nil { // leftovers of an interrupted download; only this program stages downloads
		log.Printf("could not clean staging directory: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	yt := youtube.New(func() youtube.Settings {
		d := cfg.Get().Download
		return youtube.Settings{YtDlpPath: d.YtDlpPath, CookiesFile: d.CookiesFile}
	})
	svc := syncer.New(ctx, cfg, lib, podsync.NewRegistry(yt), status.New())
	cfg.OnChange(svc.ConfigChanged)
	go svc.Run()

	public := newHTTPServer(c.Server.ListenAddr, publish.NewServer(cfg, lib).Handler())
	api := newHTTPServer(apiAddr(c), rest.NewServer(svc).Handler())
	servers := []*http.Server{public, api}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errc <- s.ListenAndServe() }()
	}
	log.Printf("syncr %s: feeds and media on %s, control API on %s (config %s, data %s, %d feeds)",
		version, public.Addr, api.Addr, cfgPath, c.Server.DataDir, len(c.Feeds))

	shutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for _, s := range servers {
			wg.Add(1)
			go func() { defer wg.Done(); _ = s.Shutdown(shutCtx) }()
		}
		wg.Wait()
	}
	select {
	case err := <-errc:
		shutdown()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Print("shutting down…")
		shutdown()
	}
	return nil
}
