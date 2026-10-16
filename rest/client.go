package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"podsync"
)

var _ podsync.Syncer = (*Client)(nil)

// Client implements podsync.Syncer by calling a syncr REST API.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient returns a client for the API at base (e.g. http://localhost:8081).
// A nil http.Client uses one without an overall timeout: calls are bounded by
// the caller's context (some, like Resolve, legitimately take a while).
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: hc}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("syncr unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted && resp.ContentLength == 0 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode syncr response: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var eb errorBody
	if json.Unmarshal(b, &eb) == nil && eb.Error.Message != "" {
		return podsync.ErrorFromKind(eb.Error.Kind, eb.Error.Message)
	}
	return fmt.Errorf("syncr returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func esc(s string) string { return url.PathEscape(s) }

func (c *Client) Feeds(ctx context.Context) (out podsync.FeedList, err error) {
	err = c.do(ctx, "GET", "/feeds", nil, nil, &out)
	return
}

func (c *Client) Feed(ctx context.Context, id string) (out podsync.FeedDetail, err error) {
	err = c.do(ctx, "GET", "/feeds/"+esc(id), nil, nil, &out)
	return
}

func (c *Client) Episodes(ctx context.Context, feedID, q string) (out []podsync.EpisodeView, err error) {
	err = c.do(ctx, "GET", "/feeds/"+esc(feedID)+"/episodes", url.Values{"q": {q}}, nil, &out)
	return
}

func (c *Client) Episode(ctx context.Context, feedID string, hint podsync.EpisodeInfo) (out podsync.EpisodeView, err error) {
	q := url.Values{
		"title": {hint.Title}, "thumb": {hint.Thumbnail}, "channel": {hint.Channel},
		"duration": {strconv.Itoa(hint.DurationSec)}, "views": {strconv.FormatInt(hint.ViewCount, 10)},
	}
	err = c.do(ctx, "GET", "/feeds/"+esc(feedID)+"/episodes/"+esc(hint.ID), q, nil, &out)
	return
}

func (c *Client) Older(ctx context.Context, feedID string, offset int) (out podsync.OlderPage, err error) {
	err = c.do(ctx, "GET", "/feeds/"+esc(feedID)+"/older", url.Values{"offset": {strconv.Itoa(offset)}}, nil, &out)
	return
}

func (c *Client) SearchLibrary(ctx context.Context, q string) (out podsync.SearchHits, err error) {
	err = c.do(ctx, "GET", "/search", url.Values{"q": {q}}, nil, &out)
	return
}

func (c *Client) Resolve(ctx context.Context, ref string) (out podsync.FeedInfo, err error) {
	err = c.do(ctx, "POST", "/resolve", nil, resolveRequest{Ref: ref}, &out)
	return
}

func (c *Client) SearchSource(ctx context.Context, query string, kind podsync.SearchKind) (out []podsync.SearchResult, err error) {
	err = c.do(ctx, "GET", "/discover", url.Values{"q": {query}, "kind": {string(kind)}}, nil, &out)
	return
}

func (c *Client) AddFeed(ctx context.Context, req podsync.AddFeedRequest) (out podsync.FeedSummary, err error) {
	err = c.do(ctx, "POST", "/feeds", nil, req, &out)
	return
}

func (c *Client) UpdateFeed(ctx context.Context, id string, upd podsync.FeedSettings) (out podsync.FeedDetail, err error) {
	err = c.do(ctx, "PUT", "/feeds/"+esc(id), nil, upd, &out)
	return
}

func (c *Client) DeleteFeed(ctx context.Context, id string, deleteFiles bool) error {
	return c.do(ctx, "DELETE", "/feeds/"+esc(id), url.Values{"delete_files": {strconv.FormatBool(deleteFiles)}}, nil, nil)
}

func (c *Client) DeleteOrphan(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/orphans/"+esc(id), nil, nil, nil)
}

func (c *Client) Settings(ctx context.Context) (out podsync.Settings, err error) {
	err = c.do(ctx, "GET", "/settings", nil, nil, &out)
	return
}

func (c *Client) UpdateSettings(ctx context.Context, s podsync.Settings) (out podsync.SettingsResult, err error) {
	err = c.do(ctx, "PUT", "/settings", nil, s, &out)
	return
}

func (c *Client) Pull(ctx context.Context, feedID string) error {
	if feedID == "" {
		return c.do(ctx, "POST", "/pull", nil, nil, nil)
	}
	return c.do(ctx, "POST", "/feeds/"+esc(feedID)+"/pull", nil, nil, nil)
}

func (c *Client) Download(ctx context.Context, feedID string, ep podsync.EpisodeInfo) error {
	return c.do(ctx, "POST", "/feeds/"+esc(feedID)+"/episodes/"+esc(ep.ID)+"/download", nil, ep, nil)
}

func (c *Client) Status(ctx context.Context) (out podsync.Status, err error) {
	err = c.do(ctx, "GET", "/status", nil, nil, &out)
	return
}

func (c *Client) DismissIssue(ctx context.Context, key string) error {
	return c.do(ctx, "POST", "/status/dismiss", nil, dismissRequest{Key: key}, nil)
}
