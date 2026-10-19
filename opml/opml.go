// Package opml renders an OPML 2.0 subscription list of podcast feeds, for
// podcast apps that import subscriptions in bulk.
package opml

import (
	"encoding/xml"
)

type doc struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    head     `xml:"head"`
	Body    body     `xml:"body"`
}

type head struct {
	Title string `xml:"title"`
}

type body struct {
	Outlines []outline `xml:"outline"`
}

type outline struct {
	Text    string `xml:"text,attr"`
	Title   string `xml:"title,attr"`
	Type    string `xml:"type,attr"`
	XMLURL  string `xml:"xmlUrl,attr"`
	HTMLURL string `xml:"htmlUrl,attr,omitempty"`
}

// Feed is one podcast feed listed in the OPML document.
type Feed struct {
	Title   string
	XMLURL  string // the feed's RSS address
	HTMLURL string // the feed's human-facing page, optional
}

// Build renders the OPML document listing feeds.
func Build(title string, feeds []Feed) ([]byte, error) {
	d := doc{
		Version: "2.0",
		Head:    head{Title: title},
	}
	for _, f := range feeds {
		d.Body.Outlines = append(d.Body.Outlines, outline{
			Text: f.Title, Title: f.Title, Type: "rss", XMLURL: f.XMLURL, HTMLURL: f.HTMLURL,
		})
	}
	out, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}
