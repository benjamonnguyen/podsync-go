package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"podsync"
)

// parseMinDuration reads a duration like "60s", "2m" or a bare number of
// seconds; empty means "not set" (nil), "0" means no minimum.
func parseMinDuration(v string) (*podsync.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		v = strconv.Itoa(n) + "s"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return nil, errors.New("min_duration must look like 61s or 2m (0 for no minimum)")
	}
	pd := podsync.Duration(d)
	return &pd, nil
}

func intField(r *http.Request, name string, dst *int, errs *[]error) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
	if err != nil {
		*errs = append(*errs, errors.New(name+" must be a whole number"))
		return
	}
	*dst = n
}

// applySettingsForm copies the settings form into s. Parse problems are
// returned; value validation happens in the sync service.
func applySettingsForm(s *Settings, r *http.Request) error {
	var errs []error
	_ = r.ParseForm()
	s.Download.SelfUpdate = r.FormValue("self_update") == "on"
	intField(r, "concurrency", &s.Download.Concurrency, &errs)
	d, err := time.ParseDuration(strings.TrimSpace(r.FormValue("update_interval")))
	if err != nil {
		errs = append(errs, errors.New("update_interval must look like 30m or 2h"))
	} else {
		s.Download.UpdateInterval = podsync.Duration(d)
	}
	s.Defaults.Format = r.FormValue("format")
	if d, err := parseMinDuration(r.FormValue("min_duration")); err != nil {
		errs = append(errs, err)
	} else if d == nil {
		errs = append(errs, errors.New("min_duration is required (0 for no minimum)"))
	} else {
		s.Defaults.MinDuration = *d
	}
	intField(r, "max_resolution", &s.Defaults.MaxResolution, &errs)
	intField(r, "cleanup_keep_last", &s.Defaults.CleanupKeepLast, &errs)
	intField(r, "page_size", &s.Defaults.PageSize, &errs)
	return errors.Join(errs...)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, cur Settings, flash, errText string) {
	s.render(w, r, "settings", map[string]any{"Title": "Settings", "Cfg": cur, "ConfigPath": cur.ConfigPath, "Flash": flash, "Err": errText})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, s.Settings(r.Context()), "", "")
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	attempt := s.Settings(r.Context())
	parseErr := applySettingsForm(&attempt, r)
	if parseErr != nil {
		s.renderSettings(w, r, attempt, "", parseErr.Error())
		return
	}
	res, err := s.UpdateSettings(r.Context(), attempt)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.renderSettings(w, r, attempt, "", err.Error())
		return
	}
	flash := "Saved. Changes are applied."
	switch {
	case res.ApplyErr != nil:
		flash = res.ApplyErr.Error()
	case res.RestartRequired:
		flash = "Saved. The sync service's listen address or data directory changed in the config file and only take effect after a restart."
	}
	s.renderSettings(w, r, res.Settings, flash, "")
}

// ---- per-feed overrides ----

type feedSettingsView struct {
	ID, Name, Kind  string
	Format          string
	MaxResolution   string
	MinDuration     string
	TitleInclude    string
	TitleExclude    string
	CleanupKeepLast string
	PageSize        string
	Defaults        podsync.Options
}

// seconds renders a duration as whole seconds ("60s") for the forms.
func seconds(d podsync.Duration) string {
	if d.Std()%time.Second == 0 {
		return strconv.FormatInt(int64(d.Std()/time.Second), 10) + "s"
	}
	return d.String()
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

func feedSettingsFrom(id, kind string, fs FeedSettings, d podsync.Options) feedSettingsView {
	v := feedSettingsView{
		ID: id, Name: fs.Name, Kind: kind, Format: sval(fs.Format), MaxResolution: itoa(fs.MaxResolution),
		TitleInclude: fs.TitleInclude, TitleExclude: fs.TitleExclude,
		CleanupKeepLast: itoa(fs.CleanupKeepLast), PageSize: itoa(fs.PageSize),
		Defaults: d,
	}
	if fs.MinDuration != nil {
		v.MinDuration = seconds(*fs.MinDuration)
	}
	return v
}

func settingsFromFeed(f podsync.Feed) FeedSettings {
	return FeedSettings{
		Name: f.Name, Format: f.Format, MaxResolution: f.MaxResolution, MinDuration: f.MinDuration,
		TitleInclude: f.TitleInclude, TitleExclude: f.TitleExclude,
		CleanupKeepLast: f.CleanupKeepLast, PageSize: f.PageSize,
	}
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

func feedSettingsFromForm(r *http.Request, current string) (FeedSettings, error) {
	var errs []error
	_ = r.ParseForm()
	fs := FeedSettings{Name: current}
	if n := strings.TrimSpace(r.FormValue("name")); n != "" {
		fs.Name = n
	}
	fs.Format = optString(r, "format")
	fs.MaxResolution = optInt(r, "max_resolution", &errs)
	if d, err := parseMinDuration(r.FormValue("min_duration")); err != nil {
		errs = append(errs, err)
	} else {
		fs.MinDuration = d
	}
	fs.TitleInclude = strings.TrimSpace(r.FormValue("title_include"))
	fs.TitleExclude = strings.TrimSpace(r.FormValue("title_exclude"))
	fs.CleanupKeepLast = optInt(r, "cleanup_keep_last", &errs)
	fs.PageSize = optInt(r, "page_size", &errs)
	return fs, errors.Join(errs...)
}

func (s *Server) renderFeedSettings(w http.ResponseWriter, r *http.Request, d FeedDetail, fs FeedSettings, flash, errText string) {
	s.render(w, r, "feed_settings", map[string]any{
		"Title": d.Name + " settings", "F": feedSettingsFrom(d.ID, d.Kind, fs, d.Defaults), "Flash": flash, "Err": errText,
	})
}

func (s *Server) handleFeedSettings(w http.ResponseWriter, r *http.Request) {
	d, err := s.Feed(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderFeedSettings(w, r, d, settingsFromFeed(d.Overrides), "", "")
}

func (s *Server) handleFeedSettingsSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := s.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fs, parseErr := feedSettingsFromForm(r, d.Name)
	if parseErr != nil {
		s.renderFeedSettings(w, r, d, fs, "", parseErr.Error())
		return
	}
	updated, err := s.UpdateFeed(r.Context(), id, fs)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.renderFeedSettings(w, r, d, fs, "", err.Error())
		return
	}
	s.renderFeedSettings(w, r, updated, settingsFromFeed(updated.Overrides), "Saved. Changes apply to the next update.", "")
}

// ---- deleting ----

func (s *Server) handleDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	d, err := s.Feed(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "delete", map[string]any{"Title": "Delete " + d.Name, "Card": d.FeedSummary})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteFeed(r.Context(), r.PathValue("id"), r.FormValue("delete_files") == "on"); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/")
}

func (s *Server) handleOrphanDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteOrphan(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/")
}
