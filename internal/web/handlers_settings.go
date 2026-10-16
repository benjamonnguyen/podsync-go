package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"podsync-go/internal/config"
)

type settingsView struct {
	Cfg config.Config
}

func intField(r *http.Request, name string, dst *int, errs *[]error) {
	v := strings.TrimSpace(r.FormValue(name))
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, errors.New(name+" must be a whole number"))
		return
	}
	*dst = n
}

// applySettingsForm copies the global settings form into c. Parse problems
// are returned; value validation happens in config.Validate.
func applySettingsForm(c *config.Config, r *http.Request) error {
	var errs []error
	_ = r.ParseForm()
	c.Server.ListenAddr = r.FormValue("listen_addr")
	c.Server.BaseURL = r.FormValue("base_url")
	c.Server.DataDir = r.FormValue("data_dir")
	c.Download.YtDlpPath = r.FormValue("yt_dlp_path")
	c.Download.CookiesFile = r.FormValue("cookies_file")
	c.Download.SelfUpdate = r.FormValue("self_update") == "on"
	intField(r, "concurrency", &c.Download.Concurrency, &errs)
	d, err := time.ParseDuration(strings.TrimSpace(r.FormValue("update_interval")))
	if err != nil {
		errs = append(errs, errors.New("update_interval must look like 30m or 2h"))
	} else {
		c.Download.UpdateInterval = config.Duration(d)
	}
	c.Defaults.Format = r.FormValue("format")
	c.Defaults.ExcludeShorts = r.FormValue("exclude_shorts") == "on"
	c.Defaults.TitleInclude = r.FormValue("title_include")
	c.Defaults.TitleExclude = r.FormValue("title_exclude")
	intField(r, "max_resolution", &c.Defaults.MaxResolution, &errs)
	intField(r, "cleanup_after_days", &c.Defaults.CleanupAfterDays, &errs)
	intField(r, "cleanup_keep_last", &c.Defaults.CleanupKeepLast, &errs)
	intField(r, "page_size", &c.Defaults.PageSize, &errs)
	return errors.Join(errs...)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings", map[string]any{"Title": "Settings", "Cfg": s.cfg.Get(), "ConfigPath": s.cfg.Path()})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	before := s.cfg.Get()
	err := s.cfg.Update(func(c *config.Config) error { return applySettingsForm(c, r) })
	if err != nil {
		shown := s.cfg.Get()
		_ = applySettingsForm(&shown, r)
		s.render(w, "settings", map[string]any{"Title": "Settings", "Cfg": shown, "Err": err.Error(), "ConfigPath": s.cfg.Path()})
		return
	}
	after := s.cfg.Get()
	flash := "Saved. Changes are applied."
	if before.Server.ListenAddr != after.Server.ListenAddr || before.Server.DataDir != after.Server.DataDir {
		flash = "Saved. The listen address and data directory only take effect after a restart."
	}
	s.render(w, "settings", map[string]any{"Title": "Settings", "Cfg": after, "Flash": flash, "ConfigPath": s.cfg.Path()})
}

// ---- per-feed overrides ----

type feedSettingsView struct {
	ID, Name, Kind   string
	Format           string
	MaxResolution    string
	ExcludeShorts    string // "", "yes", "no"
	TitleInclude     string
	TitleExclude     string
	CleanupAfterDays string
	CleanupKeepLast  string
	PageSize         string
	Defaults         config.Options
}

func itoa(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func sval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func feedSettingsFrom(f config.Feed, d config.Options) feedSettingsView {
	v := feedSettingsView{
		ID: f.ID, Name: f.Name, Kind: f.Kind, Format: sval(f.Format), MaxResolution: itoa(f.MaxResolution),
		TitleInclude: sval(f.TitleInclude), TitleExclude: sval(f.TitleExclude),
		CleanupAfterDays: itoa(f.CleanupAfterDays), CleanupKeepLast: itoa(f.CleanupKeepLast), PageSize: itoa(f.PageSize),
		Defaults: d,
	}
	if f.ExcludeShorts != nil {
		v.ExcludeShorts = "no"
		if *f.ExcludeShorts {
			v.ExcludeShorts = "yes"
		}
	}
	return v
}

func optString(r *http.Request, name string) *string {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return nil
	}
	return &v
}

func optInt(r *http.Request, name string, errs *[]error) *int {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, errors.New(name+" must be a whole number"))
		return nil
	}
	return &n
}

func applyFeedForm(f *config.Feed, r *http.Request) error {
	var errs []error
	_ = r.ParseForm()
	if n := strings.TrimSpace(r.FormValue("name")); n != "" {
		f.Name = n
	}
	f.Format = optString(r, "format")
	f.MaxResolution = optInt(r, "max_resolution", &errs)
	switch r.FormValue("exclude_shorts") {
	case "yes":
		t := true
		f.ExcludeShorts = &t
	case "no":
		t := false
		f.ExcludeShorts = &t
	default:
		f.ExcludeShorts = nil
	}
	f.TitleInclude = optString(r, "title_include")
	f.TitleExclude = optString(r, "title_exclude")
	f.CleanupAfterDays = optInt(r, "cleanup_after_days", &errs)
	f.CleanupKeepLast = optInt(r, "cleanup_keep_last", &errs)
	f.PageSize = optInt(r, "page_size", &errs)
	return errors.Join(errs...)
}

func (s *Server) handleFeedSettings(w http.ResponseWriter, r *http.Request) {
	cfg, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	s.render(w, "feed_settings", map[string]any{"Title": f.Name + " settings", "F": feedSettingsFrom(f, cfg.Defaults)})
}

func (s *Server) handleFeedSettingsSave(w http.ResponseWriter, r *http.Request) {
	cfg, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		for i := range c.Feeds {
			if c.Feeds[i].ID == f.ID {
				return applyFeedForm(&c.Feeds[i], r)
			}
		}
		return errors.New("feed no longer exists")
	})
	if err != nil {
		shown := f
		_ = applyFeedForm(&shown, r)
		s.render(w, "feed_settings", map[string]any{"Title": f.Name + " settings", "F": feedSettingsFrom(shown, cfg.Defaults), "Err": err.Error()})
		return
	}
	cfg = s.cfg.Get()
	f, _ = cfg.Feed(f.ID)
	s.render(w, "feed_settings", map[string]any{"Title": f.Name + " settings", "F": feedSettingsFrom(f, cfg.Defaults), "Flash": "Saved. Changes apply to the next update."})
}

// ---- deleting ----

func (s *Server) handleDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	cfg, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	s.render(w, "delete", map[string]any{"Title": "Delete " + f.Name, "Card": s.card(cfg, f)})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	_, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		kept := c.Feeds[:0:0]
		for _, x := range c.Feeds {
			if x.ID != f.ID {
				kept = append(kept, x)
			}
		}
		c.Feeds = kept
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.svc.ForgetFeed(f.ID)
	if r.FormValue("delete_files") == "on" {
		if err := s.lib.DeleteFeed(f.ID); err != nil {
			http.Error(w, "feed removed from config but deleting files failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	redirect(w, r, "/")
}

func (s *Server) handleOrphanDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, configured := s.cfg.Get().Feed(id); configured {
		http.Error(w, "feed is still configured", http.StatusBadRequest)
		return
	}
	if err := s.lib.DeleteFeed(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/")
}
