package youtube

// Listing and resolving feeds go through the YouTube Data API v3; downloads
// and channel name search still use yt-dlp. Quota costs (10,000 units a day
// by default): every call made here is 1 unit; a feed check is one
// playlistItems page per 50 episodes plus one videos call per 50 episodes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"podsync"
)

// DefaultAPIBase is the YouTube Data API v3 endpoint.
const DefaultAPIBase = "https://www.googleapis.com/youtube/v3"

const (
	apiTimeout  = 30 * time.Second
	apiPageSize = 50 // the API's maximum maxResults
	// maxReverseItems bounds how much of a playlist a reversed (oldest-first)
	// feed reads on each check: it has to page to the end to find the newest.
	maxReverseItems = 5000
	maxAPIBody      = 8 << 20
)

type apiErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// apiError turns an API error response into a message that says what to do.
func apiError(status int, body []byte) error {
	var b apiErrorBody
	_ = json.Unmarshal(body, &b)
	reasons := map[string]bool{}
	for _, e := range b.Error.Errors {
		reasons[e.Reason] = true
	}
	for _, d := range b.Error.Details {
		reasons[d.Reason] = true
	}
	msg := strings.TrimSpace(b.Error.Message)
	switch {
	case reasons["quotaExceeded"] || reasons["dailyLimitExceeded"]:
		return errors.New("the YouTube API key's daily quota is used up; listing resumes when it resets at midnight Pacific time")
	case reasons["rateLimitExceeded"] || reasons["userRateLimitExceeded"]:
		return errors.New("rate limited by the YouTube API; it will be retried on the next update")
	case reasons["keyInvalid"] || reasons["API_KEY_INVALID"] || strings.Contains(msg, "API key not valid"):
		return errors.New("the YouTube API key is not valid; check PODSYNC_YOUTUBE_API_KEY")
	case reasons["accessNotConfigured"] || reasons["SERVICE_DISABLED"]:
		return errors.New("the YouTube Data API v3 is not enabled in the API key's Google Cloud project")
	case reasons["playlistNotFound"] || reasons["channelNotFound"] || reasons["videoNotFound"]:
		return podsync.NotFound("YouTube API: %s", firstNonEmpty(msg, "not found"))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("YouTube API error (%d): %s", status, msg)
}

// apiGet calls one API endpoint and decodes its JSON response into out.
func (y *YouTube) apiGet(ctx context.Context, endpoint string, params url.Values, out any) error {
	s := y.settings()
	if s.APIKey == "" {
		return errors.New("no YouTube API key is set (PODSYNC_YOUTUBE_API_KEY)")
	}
	base := firstNonEmpty(s.APIBase, DefaultAPIBase)
	params.Set("key", s.APIKey)
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := y.http.Do(req)
	if err != nil {
		// A *url.Error quotes the request URL, which carries the key.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("YouTube API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return fmt.Errorf("YouTube API response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("could not parse the YouTube API response: %w", err)
	}
	return nil
}

// ---- response shapes (only the fields podsync uses) ----

type apiThumbs map[string]struct {
	URL string `json:"url"`
}

// best returns the largest thumbnail on offer.
func (t apiThumbs) best() string {
	for _, k := range []string{"maxres", "standard", "high", "medium", "default"} {
		if th, ok := t[k]; ok && th.URL != "" {
			return th.URL
		}
	}
	return ""
}

type apiList[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

type apiChannel struct {
	ID      string `json:"id"`
	Snippet struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		CustomURL   string    `json:"customUrl"` // the @handle
		Thumbnails  apiThumbs `json:"thumbnails"`
	} `json:"snippet"`
	Statistics struct {
		SubscriberCount       string `json:"subscriberCount"`
		HiddenSubscriberCount bool   `json:"hiddenSubscriberCount"`
	} `json:"statistics"`
	ContentDetails struct {
		RelatedPlaylists struct {
			Uploads string `json:"uploads"`
		} `json:"relatedPlaylists"`
	} `json:"contentDetails"`
}

type apiPlaylist struct {
	ID      string `json:"id"`
	Snippet struct {
		Title       string    `json:"title"`
		Description string    `json:"description"`
		Thumbnails  apiThumbs `json:"thumbnails"`
	} `json:"snippet"`
}

type apiPlaylistItem struct {
	Snippet struct {
		Title                  string    `json:"title"`
		Thumbnails             apiThumbs `json:"thumbnails"`
		VideoOwnerChannelTitle string    `json:"videoOwnerChannelTitle"`
	} `json:"snippet"`
	ContentDetails struct {
		VideoID          string `json:"videoId"`
		VideoPublishedAt string `json:"videoPublishedAt"`
	} `json:"contentDetails"`
	Status struct {
		PrivacyStatus string `json:"privacyStatus"`
	} `json:"status"`
}

type apiVideo struct {
	ID      string `json:"id"`
	Snippet struct {
		Title                string    `json:"title"`
		ChannelID            string    `json:"channelId"`
		ChannelTitle         string    `json:"channelTitle"`
		PublishedAt          string    `json:"publishedAt"`
		LiveBroadcastContent string    `json:"liveBroadcastContent"` // "live" | "upcoming" | "none"
		Thumbnails           apiThumbs `json:"thumbnails"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration string `json:"duration"` // ISO 8601, e.g. PT1H2M3S
	} `json:"contentDetails"`
	Statistics struct {
		ViewCount string `json:"viewCount"`
	} `json:"statistics"`
}

// ---- lookups ----

// channelLookup names a channel the way the API can find it: by "id",
// "forHandle" or "forUsername".
type channelLookup struct{ by, value string }

var (
	channelPathIDRe = regexp.MustCompile(`^/channel/([^/]+)`)
	handlePathRe    = regexp.MustCompile(`^/(@[^/]+)`)
	userPathRe      = regexp.MustCompile(`^/user/([^/]+)`)
	customPathRe    = regexp.MustCompile(`^/c/([^/]+)`)
)

// channelLookupOf finds the channel in a channel URL (as parseRef or a
// configured feed has it). Legacy /c/ custom URLs have no API lookup, so they
// are resolved to a channel ID with yt-dlp first.
func (y *YouTube) channelLookupOf(ctx context.Context, rawURL string) (channelLookup, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return channelLookup{}, podsync.Invalid("not a channel URL: %s", rawURL)
	}
	path, _ := url.PathUnescape(u.EscapedPath())
	switch {
	case channelPathIDRe.MatchString(path):
		return channelLookup{"id", channelPathIDRe.FindStringSubmatch(path)[1]}, nil
	case handlePathRe.MatchString(path):
		return channelLookup{"forHandle", handlePathRe.FindStringSubmatch(path)[1]}, nil
	case userPathRe.MatchString(path):
		return channelLookup{"forUsername", userPathRe.FindStringSubmatch(path)[1]}, nil
	case customPathRe.MatchString(path):
		id, err := y.channelIDWithYtdlp(ctx, rawURL)
		if err != nil {
			return channelLookup{}, err
		}
		return channelLookup{"id", id}, nil
	}
	return channelLookup{}, podsync.Invalid("not a channel URL: %s", rawURL)
}

// channelIDWithYtdlp resolves a URL the API cannot look up (a /c/ custom URL)
// to its channel ID.
func (y *YouTube) channelIDWithYtdlp(ctx context.Context, u string) (string, error) {
	out, err := y.run(ctx, resolveTimeout, append(y.baseArgs(), "-J", "--flat-playlist", "--playlist-end", "1", "--", u)...)
	if err != nil {
		return "", err
	}
	var p ytEntry
	if err := parseJSON(out, &p); err != nil {
		return "", err
	}
	id := firstNonEmpty(p.ChannelID, p.ID)
	if !channelIDRe.MatchString(id) {
		return "", errors.New("could not find the channel ID of " + u)
	}
	return id, nil
}

func (y *YouTube) channel(ctx context.Context, l channelLookup) (apiChannel, error) {
	var r apiList[apiChannel]
	if err := y.apiGet(ctx, "channels", url.Values{"part": {"snippet,contentDetails,statistics"}, l.by: {l.value}}, &r); err != nil {
		return apiChannel{}, err
	}
	if len(r.Items) == 0 {
		return apiChannel{}, podsync.NotFound("no YouTube channel found for %s", l.value)
	}
	return r.Items[0], nil
}

// uploadsPlaylist returns the ID of the playlist holding a channel's uploads,
// newest first. For a channel ID that is "UU" + the ID's tail and costs
// nothing; a handle or username is looked up once and remembered.
func (y *YouTube) uploadsPlaylist(ctx context.Context, rawURL string) (string, error) {
	l, err := y.channelLookupOf(ctx, rawURL)
	if err != nil {
		return "", err
	}
	if l.by == "id" && channelIDRe.MatchString(l.value) {
		return "UU" + l.value[2:], nil
	}
	key := l.by + ":" + strings.ToLower(l.value)
	y.mu.Lock()
	pl, ok := y.uploads[key]
	y.mu.Unlock()
	if ok {
		return pl, nil
	}
	ch, err := y.channel(ctx, l)
	if err != nil {
		return "", err
	}
	pl = ch.ContentDetails.RelatedPlaylists.Uploads
	if pl == "" {
		return "", fmt.Errorf("YouTube did not report an uploads playlist for %s", l.value)
	}
	y.mu.Lock()
	y.uploads[key] = pl
	y.mu.Unlock()
	return pl, nil
}

// playlistItems reads a playlist from the start until it has at least want
// items (or all of it, up to maxReverseItems, when want < 0).
func (y *YouTube) playlistItems(ctx context.Context, playlistID string, want int) ([]apiPlaylistItem, error) {
	limit := want
	if limit < 0 || limit > maxReverseItems {
		limit = maxReverseItems
	}
	var items []apiPlaylistItem
	token := ""
	for len(items) < limit {
		params := url.Values{"part": {"snippet,contentDetails,status"}, "playlistId": {playlistID}, "maxResults": {strconv.Itoa(apiPageSize)}}
		if token != "" {
			params.Set("pageToken", token)
		}
		var r apiList[apiPlaylistItem]
		if err := y.apiGet(ctx, "playlistItems", params, &r); err != nil {
			return nil, err
		}
		items = append(items, r.Items...)
		if r.NextPageToken == "" || len(r.Items) == 0 {
			break
		}
		token = r.NextPageToken
	}
	return items, nil
}

// videos fetches details for ids, 50 per call. Videos the API does not
// return (private, deleted) are simply missing from the map.
func (y *YouTube) videos(ctx context.Context, ids []string) (map[string]apiVideo, error) {
	out := make(map[string]apiVideo, len(ids))
	for start := 0; start < len(ids); start += apiPageSize {
		end := min(start+apiPageSize, len(ids))
		var r apiList[apiVideo]
		params := url.Values{"part": {"snippet,contentDetails,statistics"}, "id": {strings.Join(ids[start:end], ",")}, "maxResults": {strconv.Itoa(apiPageSize)}}
		if err := y.apiGet(ctx, "videos", params, &r); err != nil {
			return nil, err
		}
		for _, v := range r.Items {
			out[v.ID] = v
		}
	}
	return out, nil
}

// ---- conversions ----

func channelInfo(ch apiChannel) podsync.FeedInfo {
	info := podsync.FeedInfo{
		Source: Kind, Kind: podsync.FeedChannel, ID: ch.ID, URL: channelURL(ch.ID),
		Name: firstNonEmpty(ch.Snippet.Title, ch.ID), Description: strings.TrimSpace(ch.Snippet.Description),
		ImageURL: ch.Snippet.Thumbnails.best(),
	}
	if strings.HasPrefix(ch.Snippet.CustomURL, "@") {
		info.Handle = ch.Snippet.CustomURL
	}
	if !ch.Statistics.HiddenSubscriberCount {
		info.Subscribers, _ = strconv.ParseInt(ch.Statistics.SubscriberCount, 10, 64)
	}
	return info
}

func playlistInfo(p apiPlaylist) podsync.FeedInfo {
	return podsync.FeedInfo{
		Source: Kind, Kind: podsync.FeedPlaylist, ID: p.ID, URL: playlistURL(p.ID),
		Name: firstNonEmpty(p.Snippet.Title, p.ID), Description: strings.TrimSpace(p.Snippet.Description),
		ImageURL: p.Snippet.Thumbnails.best(),
	}
}

// episodeInfo combines a playlist entry with its video details (ok is false
// when the API did not return the video: it is private or deleted).
func episodeInfo(it apiPlaylistItem, v apiVideo, ok bool) podsync.EpisodeInfo {
	id := it.ContentDetails.VideoID
	ep := podsync.EpisodeInfo{
		ID: id, Title: strings.TrimSpace(it.Snippet.Title), URL: "https://www.youtube.com/watch?v=" + id,
		Thumbnail: it.Snippet.Thumbnails.best(), Channel: it.Snippet.VideoOwnerChannelTitle,
		Published: parseAPITime(it.ContentDetails.VideoPublishedAt),
	}
	if !ok {
		ep.Unavailable = true
		return ep
	}
	ep.Title = firstNonEmpty(strings.TrimSpace(v.Snippet.Title), ep.Title)
	ep.Thumbnail = firstNonEmpty(v.Snippet.Thumbnails.best(), ep.Thumbnail)
	ep.Channel = firstNonEmpty(ep.Channel, v.Snippet.ChannelTitle)
	if ep.Published.IsZero() {
		ep.Published = parseAPITime(v.Snippet.PublishedAt)
	}
	ep.DurationSec = parseISODuration(v.ContentDetails.Duration)
	ep.ViewCount, _ = strconv.ParseInt(v.Statistics.ViewCount, 10, 64)
	ep.IsLive = v.Snippet.LiveBroadcastContent == "live" || v.Snippet.LiveBroadcastContent == "upcoming"
	return ep
}

func parseAPITime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

var isoDurationRe = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// parseISODuration reads the API's ISO 8601 durations (PT1H2M3S, P1DT2H,
// P0D) as whole seconds; anything else is 0 (unknown).
func parseISODuration(s string) int {
	m := isoDurationRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	total := 0
	for i, unit := range []int{86400, 3600, 60, 1} {
		if n, err := strconv.Atoi(m[i+1]); err == nil {
			total += n * unit
		}
	}
	return total
}
