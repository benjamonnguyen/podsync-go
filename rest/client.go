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
// the caller's context (lookups like Resolve legitimately take a while).
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
		return fmt.Errorf("sync service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode sync service response: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var eb errorBody
	if json.Unmarshal(b, &eb) == nil && eb.Error.Message != "" {
		return podsync.ErrorFromKind(eb.Error.Kind, eb.Error.Message)
	}
	return fmt.Errorf("sync service returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (c *Client) Apply(ctx context.Context) (out podsync.ApplyResult, err error) {
	err = c.do(ctx, "POST", "/apply", nil, nil, &out)
	return
}

func (c *Client) Pull(ctx context.Context, req podsync.PullRequest) error {
	return c.do(ctx, "POST", "/pull", nil, req, nil)
}

func (c *Client) Status(ctx context.Context) (out podsync.SyncStatus, err error) {
	err = c.do(ctx, "GET", "/status", nil, nil, &out)
	return
}

func (c *Client) Find(ctx context.Context, query string) (out []podsync.FeedInfo, err error) {
	err = c.do(ctx, "GET", "/find", url.Values{"q": {query}}, nil, &out)
	return
}

func (c *Client) List(ctx context.Context, feedID string, offset, limit int) (out []podsync.EpisodeInfo, err error) {
	q := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	err = c.do(ctx, "GET", "/feeds/"+url.PathEscape(feedID)+"/listing", q, nil, &out)
	return
}
