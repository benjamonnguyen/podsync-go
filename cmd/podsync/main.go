// Command podsync fetches episodes from feed sources (YouTube via yt-dlp)
// into the library on disk, publishes the podcast feeds and media files, and
// (unless PODSYNC_WEBUI_DISABLED is set) serves a web UI for managing the
// library — all from one process on one listener. The public feed/media
// endpoints (/feeds, /media, /opml.xml) are unauthenticated, since that's what podcast
// apps fetch; everything else (the management portal) requires the HTTP
// Basic credentials set by PODSYNC_USERNAME/PODSYNC_PASSWORD. Basic auth is
// sent in the clear unless PODSYNC_TLS_CERT_FILE/PODSYNC_TLS_KEY_FILE are set,
// in which case the one listener speaks HTTPS instead of HTTP. Almost every
// deployment setting is an environment variable (all prefixed PODSYNC_);
// -listen and -config also accept a command-line flag, for the common case
// of running a few instances by hand without Docker. podsync fails fast
// with a message naming what's missing or invalid.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"podsync"
	"podsync/fs"
	"podsync/publish"
	"podsync/status"
	"podsync/syncer"
	"podsync/toml"
	"podsync/youtube"
)

var version = "dev"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || v == "true"
}

// config is every deployment setting podsync needs; these are fixed for the
// life of the process and never edited through the web UI. All but -listen
// and -config come from PODSYNC_-prefixed environment variables only.
type config struct {
	path          string
	listenAddr    string
	baseURL       string
	dataDir       string
	cookies       string
	ytdlpPath     string
	webuiDisabled bool
	username      string
	password      string
	tlsCertFile   string
	tlsKeyFile    string
}

func configFromEnv() config {
	listen := flag.String("listen", envOr("PODSYNC_LISTEN_ADDR", ":8080"), "address to serve feeds, media and (unless disabled) the web UI on (env PODSYNC_LISTEN_ADDR)")
	path := flag.String("config", envOr("PODSYNC_CONFIG", "config.toml"), "path to the TOML config file holding feeds and defaults (env PODSYNC_CONFIG)")
	flag.Parse()
	return config{
		path:          *path,
		listenAddr:    *listen,
		baseURL:       os.Getenv("PODSYNC_BASE_URL"),
		dataDir:       envOr("PODSYNC_DATA_DIR", "./data"),
		cookies:       envOr("PODSYNC_COOKIES_FILE", ""),
		ytdlpPath:     envOr("PODSYNC_YTDLP_PATH", ""),
		webuiDisabled: envBool("PODSYNC_WEBUI_DISABLED", false),
		username:      os.Getenv("PODSYNC_USERNAME"),
		password:      os.Getenv("PODSYNC_PASSWORD"),
		tlsCertFile:   os.Getenv("PODSYNC_TLS_CERT_FILE"),
		tlsKeyFile:    os.Getenv("PODSYNC_TLS_KEY_FILE"),
	}
}

func (c config) tlsEnabled() bool { return c.tlsCertFile != "" }

// validate checks the deployment settings that Config.Validate used to check
// before they moved out of the file, and fails fast naming what's missing or
// invalid, before anything is listened on or downloaded.
func (c config) validate() error {
	if c.baseURL == "" {
		return errors.New("PODSYNC_BASE_URL is required (your address, e.g. http://192.168.1.10:8080)")
	}
	if u, err := url.Parse(c.baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("PODSYNC_BASE_URL must be an absolute http(s) URL, got %q", c.baseURL)
	}
	if !c.webuiDisabled && (c.username == "" || c.password == "") {
		return errors.New("PODSYNC_USERNAME and PODSYNC_PASSWORD are required unless PODSYNC_WEBUI_DISABLED is set")
	}
	if c.cookies != "" {
		if _, err := os.Stat(c.cookies); err != nil {
			return fmt.Errorf("PODSYNC_COOKIES_FILE: %w", err)
		}
	}
	// The default ("yt-dlp" on PATH) is intentionally not checked here: a missing
	// yt-dlp is reported on the status banner at runtime (podsync still serves
	// existing feeds and media without it), not a reason to refuse to start. An
	// explicit override is different — a typo there is a deployment mistake.
	if c.ytdlpPath != "" {
		if _, err := exec.LookPath(c.ytdlpPath); err != nil {
			return fmt.Errorf("PODSYNC_YTDLP_PATH %q: %w", c.ytdlpPath, err)
		}
	}
	if (c.tlsCertFile == "") != (c.tlsKeyFile == "") {
		return errors.New("PODSYNC_TLS_CERT_FILE and PODSYNC_TLS_KEY_FILE must be set together")
	}
	if c.tlsCertFile != "" {
		if _, err := os.Stat(c.tlsCertFile); err != nil {
			return fmt.Errorf("PODSYNC_TLS_CERT_FILE: %w", err)
		}
		if _, err := os.Stat(c.tlsKeyFile); err != nil {
			return fmt.Errorf("PODSYNC_TLS_KEY_FILE: %w", err)
		}
	}
	return nil
}

func main() {
	if envBool("PODSYNC_HEALTHCHECK", false) {
		tlsEnabled := os.Getenv("PODSYNC_TLS_CERT_FILE") != ""
		if err := probe(envOr("PODSYNC_LISTEN_ADDR", ":8080"), tlsEnabled); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}
	c := configFromEnv()
	if err := c.validate(); err != nil {
		log.Fatalf("error: %v", err)
	}
	if err := run(c); err != nil {
		log.Fatalf("error: %v", err)
	}
}

// probe checks that the listener answers /healthz. When TLS is enabled, the
// certificate is not re-verified: this is a loopback self-check, not a
// connection to a remote peer, and the cert may not even cover 127.0.0.1.
func probe(addr string, tlsEnabled bool) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	scheme := "http"
	client := http.Client{Timeout: 4 * time.Second}
	if tlsEnabled {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	resp, err := client.Get(scheme + "://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

// newHandler builds the single mux served on -listen/PODSYNC_LISTEN_ADDR:
// feeds and media are always public, the management portal is mounted at
// "/" behind HTTP Basic auth unless PODSYNC_WEBUI_DISABLED is set. It also
// returns a short description for the startup log.
func newHandler(c config, cfg podsync.ConfigStore, lib podsync.Library, svc podsync.Syncer) (http.Handler, string, error) {
	publishHandler := publish.NewServer(cfg, lib, c.baseURL).Handler()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("/feeds/", publishHandler)
	mux.Handle("/media/", publishHandler)
	mux.Handle("/opml.xml", publishHandler)

	webuiNote := "web UI off"
	if !c.webuiDisabled {
		ui, err := newServer(options{Config: cfg, Library: lib, Syncer: svc, BaseURL: c.baseURL})
		if err != nil {
			return nil, "", err
		}
		mux.Handle("/", basicAuth(c.username, c.password, ui.Handler()))
		webuiNote = "web UI on (Basic auth)"
	}
	return mux, webuiNote, nil
}

func run(c config) error {
	cfg, err := toml.Open(c.path)
	if err != nil {
		return err
	}

	lib := fs.New(c.dataDir)
	if err := lib.Load(); err != nil {
		return fmt.Errorf("load library from %s: %w", c.dataDir, err)
	}
	if err := lib.CleanTemp(); err != nil { // leftovers of an interrupted download; only this program stages downloads
		log.Printf("could not clean staging directory: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	yt := youtube.New(func() youtube.Settings {
		return youtube.Settings{CookiesFile: c.cookies, BinPath: c.ytdlpPath}
	})
	svc := syncer.New(ctx, cfg, lib, podsync.NewRegistry(yt), status.New())
	cfg.OnChange(svc.ConfigChanged)
	go svc.Run()

	mux, webuiNote, err := newHandler(c, cfg, lib, svc)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              c.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: media downloads to slow podcast clients can take long.
	}
	errc := make(chan error, 1)
	scheme := "http, no TLS"
	if c.tlsEnabled() {
		scheme = "https (TLS)"
		go func() { errc <- srv.ListenAndServeTLS(c.tlsCertFile, c.tlsKeyFile) }()
	} else {
		go func() { errc <- srv.ListenAndServe() }()
	}
	log.Printf("podsync %s listening on %s (%s, %s; config %s, data %s, public address %s, %d feeds)",
		version, srv.Addr, scheme, webuiNote, c.path, c.dataDir, c.baseURL, len(cfg.Get().Feeds))

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
