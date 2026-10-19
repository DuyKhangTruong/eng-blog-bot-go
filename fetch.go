package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

type item struct {
	guid      string
	title     string
	link      string
	published *time.Time
	author    string
	summary   string
	tags      []string
	// Sanitized full article HTML when the feed carries the whole post
	// (Medium, many WordPress blogs); empty when it only has an excerpt.
	contentHTML string
	wordCount   int
}

type cacheHeaders struct {
	etag         string
	lastModified string
}

type outcome struct {
	notModified bool
	items       []item
	cache       cacheHeaders
	failed      string // non-empty when the fetch failed
}

var tagRe = regexp.MustCompile(`<[^>]*>`)
var wsRe = regexp.MustCompile(`\s+`)

// Strip stray HTML tags/entities and collapse whitespace in a feed title.
func cleanTitle(raw string) string {
	s := tagRe.ReplaceAllString(raw, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// Resolve a possibly-relative link against the feed URL.
func resolveLink(feedURL, link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil || u.IsAbs() {
		return link
	}
	base, err := url.Parse(feedURL)
	if err != nil {
		return link
	}
	return base.ResolveReference(u).String()
}

func fetchFeed(client *http.Client, cfg feedCfg, cache cacheHeaders) outcome {
	req, err := http.NewRequest(http.MethodGet, cfg.url, nil)
	if err != nil {
		return outcome{failed: err.Error()}
	}
	req.Header.Set("User-Agent", userAgent)
	if cache.etag != "" {
		req.Header.Set("If-None-Match", cache.etag)
	}
	if cache.lastModified != "" {
		req.Header.Set("If-Modified-Since", cache.lastModified)
	}
	resp, err := client.Do(req)
	if err != nil {
		return outcome{failed: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return outcome{notModified: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return outcome{failed: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return outcome{failed: err.Error()}
	}
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(stripControlChars(body)))
	if err != nil {
		return outcome{failed: fmt.Sprintf("parse error: %v", err)}
	}

	var items []item
	for _, e := range feed.Items {
		link := resolveLink(cfg.url, e.Link)
		guid := strings.TrimSpace(e.GUID)
		if guid == "" {
			guid = link // same key the Python version used when no guid exists
		}
		if guid == "" {
			continue
		}
		title := cleanTitle(e.Title)
		if title == "" {
			title = "(untitled)"
		}
		var pub *time.Time
		if e.PublishedParsed != nil {
			pub = e.PublishedParsed
		} else if e.UpdatedParsed != nil {
			pub = e.UpdatedParsed
		}
		items = append(items, buildItem(e, guid, title, link, pub))
	}
	return outcome{
		items: items,
		cache: cacheHeaders{
			etag:         resp.Header.Get("ETag"),
			lastModified: resp.Header.Get("Last-Modified"),
		},
	}
}

// Some feeds (e.g. Mercari) contain ASCII control characters, which are
// illegal in XML and abort parsing. These bytes never occur inside multi-byte
// UTF-8 sequences, so dropping them byte-wise is safe.
func stripControlChars(b []byte) []byte {
	out := b[:0:0]
	for _, c := range b {
		if c >= 0x20 || c == '\t' || c == '\n' || c == '\r' {
			out = append(out, c)
		}
	}
	return out
}

// Extract the app-facing metadata (author, summary, tags, body) from a feed entry.
func buildItem(e *gofeed.Item, guid, title, link string, pub *time.Time) item {
	it := item{guid: guid, title: title, link: link, published: pub, tags: cleanTags(e.Categories)}

	if len(e.Authors) > 0 && e.Authors[0] != nil {
		it.author = cleanTitle(e.Authors[0].Name)
	}

	// Some feeds put the whole article in <description>, others in <content:encoded>.
	body := e.Content
	if len(htmlToText(e.Description)) > len(htmlToText(body)) {
		body = e.Description
	}
	if words := wordCount(htmlToText(body)); words >= fullArticleMinWords {
		it.contentHTML = sanitizeArticle(body, title)
		it.wordCount = words
	}

	it.summary = makeSummary(e.Description)
	if it.summary == "" {
		it.summary = makeSummary(body)
	}
	return it
}
