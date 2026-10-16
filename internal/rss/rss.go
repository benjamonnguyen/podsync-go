// Package rss renders a podcast RSS 2.0 document (with iTunes tags) for a feed.
package rss

import (
	"encoding/xml"
	"fmt"
	"mime"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"podsync-go/internal/library"
)

type Feed struct {
	ID          string
	Title       string
	Description string
	ImageURL    string
	Link        string // human-facing page for the feed
	Self        string // URL of the RSS document itself
}

type rssDoc struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Itunes  string   `xml:"xmlns:itunes,attr"`
	Atom    string   `xml:"xmlns:atom,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title       string     `xml:"title"`
	Link        string     `xml:"link"`
	Description string     `xml:"description"`
	Language    string     `xml:"language,omitempty"`
	AtomLink    atomLink   `xml:"atom:link"`
	ItunesImage *itunesImg `xml:"itunes:image,omitempty"`
	ItunesBlock string     `xml:"itunes:explicit"`
	LastBuild   string     `xml:"lastBuildDate,omitempty"`
	Items       []item     `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type itunesImg struct {
	Href string `xml:"href,attr"`
}

type item struct {
	Title       string     `xml:"title"`
	Link        string     `xml:"link,omitempty"`
	Description string     `xml:"description"`
	GUID        guid       `xml:"guid"`
	PubDate     string     `xml:"pubDate"`
	Enclosure   enclosure  `xml:"enclosure"`
	Duration    string     `xml:"itunes:duration,omitempty"`
	Image       *itunesImg `xml:"itunes:image,omitempty"`
}

type guid struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// MimeType maps a media file extension to an enclosure type.
func MimeType(file string) string {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".webm":
		return "video/webm"
	case ".opus", ".ogg":
		return "audio/ogg"
	case ".mkv":
		return "video/x-matroska"
	}
	if t := mime.TypeByExtension(filepath.Ext(file)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// MediaURL is the absolute URL of an episode's media file.
func MediaURL(baseURL, feedID, file string) string {
	return strings.TrimRight(baseURL, "/") + "/media/" + url.PathEscape(feedID) + "/" + url.PathEscape(file)
}

func clock(sec int) string {
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, sec%3600/60, sec%60)
}

// Build renders the document. Only downloaded episodes are included, newest first.
func Build(baseURL string, f Feed, eps []library.Episode, now time.Time) ([]byte, error) {
	ch := channel{
		Title:       f.Title,
		Link:        f.Link,
		Description: firstNonEmpty(f.Description, f.Title),
		AtomLink:    atomLink{Href: f.Self, Rel: "self", Type: "application/rss+xml"},
		ItunesBlock: "no",
		LastBuild:   now.UTC().Format(time.RFC1123Z),
	}
	if f.ImageURL != "" {
		ch.ItunesImage = &itunesImg{Href: f.ImageURL}
	}
	sorted := append([]library.Episode(nil), eps...)
	library.SortNewestFirst(sorted)
	for _, ep := range sorted {
		if ep.Status != library.StatusDownloaded || ep.File == "" {
			continue
		}
		it := item{
			Title:       ep.Title,
			Link:        ep.URL,
			Description: firstNonEmpty(ep.Description, ep.Title),
			GUID:        guid{Value: f.ID + ":" + ep.ID},
			PubDate:     ep.Published.UTC().Format(time.RFC1123Z),
			Enclosure:   enclosure{URL: MediaURL(baseURL, f.ID, ep.File), Length: ep.Size, Type: MimeType(ep.File)},
		}
		if ep.DurationSec > 0 {
			it.Duration = clock(ep.DurationSec)
		}
		switch {
		case ep.ThumbFile != "":
			it.Image = &itunesImg{Href: MediaURL(baseURL, f.ID, ep.ThumbFile)}
		case ep.Thumbnail != "":
			it.Image = &itunesImg{Href: ep.Thumbnail}
		}
		ch.Items = append(ch.Items, it)
	}
	doc := rssDoc{
		Version: "2.0",
		Itunes:  "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: ch,
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
