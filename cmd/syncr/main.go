// Command syncr is the sync service: it fetches episodes from feed sources
// (YouTube via yt-dlp) into the library on disk. The web UI (cmd/webui) shares
// the config file and data directory with it and talks to it over a small REST
// API to apply changes, trigger pulls, read live status and look up sources.
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
	"syscall"
	"time"

	"podsync"
	"podsync/fs"
	"podsync/rest"
	"podsync/status"
	"podsync/syncer"
	"podsync/toml"
	"podsync/youtube"
)

var version = "dev"

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

// probe checks the service named in the config file answers /healthz.
func probe(cfgPath string) error {
	cfg, err := toml.Open(cfgPath)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.Get().Server.ListenAddr)
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

	srv := &http.Server{
		Addr:              c.Server.ListenAddr,
		Handler:           rest.NewServer(svc).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("syncr %s listening on %s (config %s, data %s, %d feeds)", version, c.Server.ListenAddr, cfgPath, c.Server.DataDir, len(c.Feeds))

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
