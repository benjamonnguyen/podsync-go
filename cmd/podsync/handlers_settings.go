package main

import (
	"errors"
	"log"
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
	intField(r, "max_video_height", &s.Defaults.MaxVideoHeight, &errs)
	s.Defaults.AudioQuality = r.FormValue("audio_quality")
	intField(r, "cleanup_keep_last", &s.Defaults.CleanupKeepLast, &errs)
	intField(r, "page_size", &s.Defaults.PageSize, &errs)
	return errors.Join(errs...)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, cur Settings, flash, errText string) {
	s.render(w, r, "settings", map[string]any{
		"Title": "Settings", "Cfg": cur, "ConfigPath": cur.ConfigPath, "AudioExt": s.audioExt(), "Flash": flash, "Err": errText,
	})
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
	if res.ApplyErr != nil {
		flash = res.ApplyErr.Error()
	}
	s.renderSettings(w, r, res.Settings, flash, "")
}

// ---- per-feed overrides ----

type feedSettingsView struct {
	ID, Name, Kind string
	// CurrentFormat is the feed's effective format right now (the format
	// segment of the URL this settings page is on), always concrete; Format
	// below is the submitted *override* value and may be empty.
	CurrentFormat   string
	Format          string
	MaxVideoHeight  string
	AudioQuality    string
	MinDuration     string
	TitleInclude    string
	TitleExclude    string
	CleanupKeepLast string
	PageSize        string
	PlaylistSort    string
	OPMLExclude     bool
	Defaults        podsync.Options
	// AudioExt is the file extension "audio" format actually downloads as
	// right now (m4a, or mp3 if PODSYNC_MP3_ENCODING_DISABLED is not set), for
	// the Format option's label.
	AudioExt string
}

// formatChangeConfirm is shown instead of saving when a settings submission
// would change a feed's effective format: that moves it to a different (id,
// format) identity, abandoning the old one's download history. The user
// chooses whether the old format's downloaded files are deleted now or left
// on disk (its sidecars are cleared either way; see handleFeedSettingsSave).
type formatChangeConfirm struct {
	OldFormat, NewFormat string
	OldDownloaded        int
	OldBytes             int64
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

func (s *Server) feedSettingsFrom(id, name, kind, currentFormat string, fs FeedSettings, d podsync.Options) feedSettingsView {
	v := feedSettingsView{
		ID: id, Name: name, Kind: kind, CurrentFormat: currentFormat, Format: sval(fs.Format), MaxVideoHeight: itoa(fs.MaxVideoHeight),
		AudioQuality: sval(fs.AudioQuality),
		TitleInclude: fs.TitleInclude, TitleExclude: fs.TitleExclude,
		CleanupKeepLast: itoa(fs.CleanupKeepLast), PageSize: itoa(fs.PageSize),
		PlaylistSort: fs.PlaylistSort,
		OPMLExclude:  fs.OPMLExclude,
		Defaults:     d,
		AudioExt:     s.audioExt(),
	}
	if fs.MinDuration != nil {
		v.MinDuration = seconds(*fs.MinDuration)
	}
	return v
}

func settingsFromFeed(f podsync.Feed) FeedSettings {
	return FeedSettings{
		Format: f.Format, MaxVideoHeight: f.MaxVideoHeight, AudioQuality: f.AudioQuality, MinDuration: f.MinDuration,
		TitleInclude: f.TitleInclude, TitleExclude: f.TitleExclude,
		CleanupKeepLast: f.CleanupKeepLast, PageSize: f.PageSize,
		PlaylistSort: f.PlaylistSort,
		OPMLExclude:  f.OPMLExclude,
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

func feedSettingsFromForm(r *http.Request) (FeedSettings, error) {
	var errs []error
	_ = r.ParseForm()
	fs := FeedSettings{}
	fs.Format = optString(r, "format")
	fs.MaxVideoHeight = optInt(r, "max_video_height", &errs)
	fs.AudioQuality = optString(r, "audio_quality")
	if d, err := parseMinDuration(r.FormValue("min_duration")); err != nil {
		errs = append(errs, err)
	} else {
		fs.MinDuration = d
	}
	fs.TitleInclude = strings.TrimSpace(r.FormValue("title_include"))
	fs.TitleExclude = strings.TrimSpace(r.FormValue("title_exclude"))
	fs.CleanupKeepLast = optInt(r, "cleanup_keep_last", &errs)
	fs.PageSize = optInt(r, "page_size", &errs)
	if r.FormValue("playlist_sort") == "oldest" {
		fs.PlaylistSort = podsync.PlaylistSortOldest
	}
	fs.OPMLExclude = r.FormValue("opml_exclude") == "on"
	return fs, errors.Join(errs...)
}

func (s *Server) renderFeedSettings(w http.ResponseWriter, r *http.Request, d FeedDetail, fs FeedSettings, flash, errText string) {
	s.render(w, r, "feed_settings", map[string]any{
		"Title": d.Name + " settings", "F": s.feedSettingsFrom(d.ID, d.Name, d.Kind, d.Format, fs, d.Defaults), "Flash": flash, "Err": errText,
	})
}

// renderFormatChangeConfirm shows the "what happens to the old format's
// files" prompt instead of saving, with every submitted field replayed as
// hidden inputs so confirming (or keeping the files) resubmits the same change.
func (s *Server) renderFormatChangeConfirm(w http.ResponseWriter, r *http.Request, d FeedDetail, fs FeedSettings, oldFormat, newFormat string) {
	oldLF, _ := s.lib.Feed(d.ID, oldFormat)
	s.render(w, r, "feed_settings", map[string]any{
		"Title": d.Name + " settings",
		"F":     s.feedSettingsFrom(d.ID, d.Name, d.Kind, d.Format, fs, d.Defaults),
		"Confirm": formatChangeConfirm{
			OldFormat: oldFormat, NewFormat: newFormat,
			OldDownloaded: oldLF.Downloaded, OldBytes: oldLF.Bytes,
		},
	})
}

func (s *Server) handleFeedSettings(w http.ResponseWriter, r *http.Request) {
	d, err := s.Feed(r.Context(), r.PathValue("id"), r.PathValue("format"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderFeedSettings(w, r, d, settingsFromFeed(d.Overrides), "", "")
}

// handleFeedSettingsSave saves a feed's settings. If the submission changes
// the feed's effective format, that moves it to a different (id, format)
// identity: the user is asked what to do with the old one's downloaded files
// before anything is saved (renderFormatChangeConfirm), then — once
// confirmed — the old entry's sidecars are cleared (and its media too, if
// asked) right after the new identity is saved.
func (s *Server) handleFeedSettingsSave(w http.ResponseWriter, r *http.Request) {
	id, format := r.PathValue("id"), r.PathValue("format")
	d, err := s.Feed(r.Context(), id, format)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fs, parseErr := feedSettingsFromForm(r)
	if parseErr != nil {
		s.renderFeedSettings(w, r, d, fs, "", parseErr.Error())
		return
	}
	newFormat := d.Defaults.Format
	if fs.Format != nil {
		newFormat = *fs.Format
	}
	if newFormat != format && r.FormValue("confirm_format_change") != "1" {
		s.renderFormatChangeConfirm(w, r, d, fs, format, newFormat)
		return
	}
	updated, err := s.UpdateFeed(r.Context(), id, format, fs)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.renderFeedSettings(w, r, d, fs, "", err.Error())
		return
	}
	if newFormat != format {
		deleteMedia := r.FormValue("delete_old_media") == "1"
		if err := s.lib.ClearFeed(id, format, deleteMedia); err != nil {
			log.Printf("clear old (%s, %s) after format change: %v", id, format, err)
		}
	}
	s.renderFeedSettings(w, r, updated, settingsFromFeed(updated.Overrides), "Saved. Changes apply to the next update.", "")
}

// ---- deleting ----

func (s *Server) handleDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	d, err := s.Feed(r.Context(), r.PathValue("id"), r.PathValue("format"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "delete", map[string]any{"Title": "Delete " + d.Name, "Card": d.FeedSummary})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteFeed(r.Context(), r.PathValue("id"), r.PathValue("format"), r.FormValue("delete_files") == "on"); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/")
}

func (s *Server) handleOrphanDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteOrphan(r.Context(), r.PathValue("id"), r.PathValue("format")); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/")
}
