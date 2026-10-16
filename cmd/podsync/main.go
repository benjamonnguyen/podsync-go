// Command podsync serves YouTube channels and playlists as podcast feeds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"podsync-go/internal/config"
	"podsync-go/internal/library"
	"podsync-go/internal/puller"
	"podsync-go/internal/source"
	"podsync-go/internal/source/youtube"
	"podsync-go/internal/status"
	"podsync-go/internal/web"
)

var version = "dev"

func main() {
	defPath := "config.toml"
	if v := os.Getenv("PODSYNC_CONFIG"); v != "" {
		defPath = v
	}
	cfgPath := flag.String("config", defPath, "path to the TOML config file (created with defaults if missing; env PODSYNC_CONFIG)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("podsync-go", version)
		return
	}
	if err := run(*cfgPath); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Open(cfgPath)
	if err != nil {
		return err
	}
	c := cfg.Get()

	lib := library.New(c.Server.DataDir)
	if err := lib.Load(); err != nil {
		return fmt.Errorf("load library from %s: %w", c.Server.DataDir, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := status.New()
	yt := youtube.New(func() youtube.Settings {
		d := cfg.Get().Download
		return youtube.Settings{YtDlpPath: d.YtDlpPath, CookiesFile: d.CookiesFile}
	})
	reg := source.NewRegistry(yt)
	svc := puller.New(ctx, cfg, lib, reg, st)
	cfg.OnChange(svc.ConfigChanged)
	go svc.Run()

	ui, err := web.New(cfg, lib, svc, reg, st)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              c.Server.ListenAddr,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: media downloads to slow podcast clients can take long.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("podsync-go %s listening on %s (config %s, data %s, %d feeds)", version, c.Server.ListenAddr, cfgPath, c.Server.DataDir, len(c.Feeds))

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
