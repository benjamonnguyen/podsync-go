package youtube

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const fakeKey = "test-key"

// fakeAPI is a stand-in for the YouTube Data API v3 with one channel
// (UCXuqSBlHAE6Xw-yeJA0Tunw), its uploads, and a few playlists. Pages are 5
// items long whatever maxResults says, so paging is exercised.
type fakeAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

const fakePage = 5

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) count(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[endpoint]
}

func (f *fakeAPI) reset() {
	f.mu.Lock()
	f.calls = map[string]int{}
	f.mu.Unlock()
}

func apiFail(w http.ResponseWriter, code int, reason, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg, "errors": []map[string]any{{"reason": reason}}}})
}

var fakeChannel = map[string]any{
	"id": "UCXuqSBlHAE6Xw-yeJA0Tunw",
	"snippet": map[string]any{"title": "Linus Tech Tips", "description": " about ", "customUrl": "@LinusTechTips",
		"thumbnails": map[string]any{"default": map[string]any{"url": "https://yt3/default.jpg"}, "high": map[string]any{"url": "https://yt3/high.jpg"}}},
	"statistics":     map[string]any{"subscriberCount": "16900000", "hiddenSubscriberCount": false},
	"contentDetails": map[string]any{"relatedPlaylists": map[string]any{"uploads": "UUXuqSBlHAE6Xw-yeJA0Tunw"}},
}

func fakeVideoID(i int) string { return fmt.Sprintf("vid%08d", i) }

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	endpoint := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	f.calls[endpoint]++
	f.mu.Unlock()
	q := r.URL.Query()
	if q.Get("key") != fakeKey {
		apiFail(w, 400, "badRequest", "API key not valid. Please pass a valid API key.")
		return
	}
	enc := json.NewEncoder(w)
	switch endpoint {
	case "channels":
		match := q.Get("id") == "UCXuqSBlHAE6Xw-yeJA0Tunw" ||
			strings.EqualFold(strings.TrimPrefix(q.Get("forHandle"), "@"), "LinusTechTips") ||
			strings.EqualFold(q.Get("forUsername"), "linustechtips")
		items := []any{}
		if match {
			items = append(items, fakeChannel)
		}
		enc.Encode(map[string]any{"items": items})
	case "playlists":
		items := []any{}
		if q.Get("id") == "PLabcdefghijk" {
			items = append(items, map[string]any{"id": "PLabcdefghijk", "snippet": map[string]any{"title": "A Course",
				"thumbnails": map[string]any{"medium": map[string]any{"url": "https://i.ytimg.com/pl.jpg"}}}})
		}
		enc.Encode(map[string]any{"items": items})
	case "playlistItems":
		switch q.Get("playlistId") {
		case "UUXuqSBlHAE6Xw-yeJA0Tunw", "PLsomethingCourse":
		case "PLquotaquotaquota":
			apiFail(w, 403, "quotaExceeded", "The request cannot be completed because you have exceeded your quota.")
			return
		default:
			apiFail(w, 404, "playlistNotFound", "The playlist identified with the request's playlistId parameter cannot be found.")
			return
		}
		if q.Get("maxResults") != "50" {
			apiFail(w, 400, "badRequest", "podsync should always ask for 50")
			return
		}
		start, _ := strconv.Atoi(q.Get("pageToken"))
		var items []any
		for i := start + 1; i <= min(start+fakePage, 12); i++ {
			title, privacy := fmt.Sprintf("Episode %d", i), "public"
			if i == 4 {
				title, privacy = "Private video", "private"
			}
			items = append(items, map[string]any{
				"snippet":        map[string]any{"title": title, "videoOwnerChannelTitle": "Some Channel"},
				"contentDetails": map[string]any{"videoId": fakeVideoID(i), "videoPublishedAt": fmt.Sprintf("2025-01-%02dT12:00:00Z", i)},
				"status":         map[string]any{"privacyStatus": privacy},
			})
		}
		out := map[string]any{"items": items}
		if start+fakePage < 12 {
			out["nextPageToken"] = strconv.Itoa(start + fakePage)
		}
		enc.Encode(out)
	case "videos":
		var items []any
		for _, id := range strings.Split(q.Get("id"), ",") {
			if id == "jNQXAC9IVRw" {
				items = append(items, map[string]any{"id": id, "snippet": map[string]any{"channelId": "UCXuqSBlHAE6Xw-yeJA0Tunw"}})
				continue
			}
			var i int
			if _, err := fmt.Sscanf(id, "vid%08d", &i); err != nil || i == 4 {
				continue // private: the API leaves it out
			}
			dur, live := "PT10M", "none"
			switch i {
			case 2:
				dur = "PT30S"
			case 3:
				live = "upcoming"
			}
			items = append(items, map[string]any{
				"id": id,
				"snippet": map[string]any{"title": fmt.Sprintf("Episode %d", i), "channelTitle": "Some Channel", "liveBroadcastContent": live,
					"publishedAt": fmt.Sprintf("2025-01-%02dT12:00:00Z", i),
					"thumbnails":  map[string]any{"maxres": map[string]any{"url": "https://i.ytimg.com/vi/" + id + "/maxres.jpg"}}},
				"contentDetails": map[string]any{"duration": dur},
				"statistics":     map[string]any{"viewCount": strconv.Itoa(1000 + i)},
			})
		}
		enc.Encode(map[string]any{"items": items})
	default:
		apiFail(w, 404, "notFound", "no such endpoint")
	}
}
