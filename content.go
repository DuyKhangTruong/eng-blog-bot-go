package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/microcosm-cc/bluemonday"
)

const (
	// A feed body with at least this many words is treated as the full article.
	fullArticleMinWords = 250
	// Extracted pages shorter than this are probably a consent wall or an index page.
	minExtractedWords = 120
	summaryMaxChars   = 280
	wordsPerMinute    = 230
	maxTags           = 6
)

var (
	blockTagRe = regexp.MustCompile(`(?i)</?(p|div|br|li|h[1-6]|blockquote|pre|tr|figure)[^>]*>`)
	// Footers that blog platforms append to feed bodies.
	wpFooterRe     = regexp.MustCompile(`(?is)<p>\s*The post <a[^>]*>.*?</a> appeared first on <a[^>]*>.*?</a>\.?\s*</p>`)
	textFooterRe   = regexp.MustCompile(`(?i)\s*(The post .{1,300} appeared first on .{1,120}\.|Continue reading on .{1,120}»|Read more\s*»?|\[(…|\.\.\.)\])\s*$`)
	mediumPixelRe  = regexp.MustCompile(`(?i)<img[^>]+src="https?://medium\.com/_/stat[^"]*"[^>]*>`)
	leadingH1Re    = regexp.MustCompile(`(?is)^\s*(<div[^>]*>\s*)*<h1[^>]*>(.*?)</h1>`)
	articlePolicy  = newArticlePolicy()
	errNotArticle  = errors.New("no readable article on page")
	errNotHTMLPage = errors.New("not an HTML page")
)

func newArticlePolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("figure", "figcaption")
	p.AllowAttrs("width", "height").OnElements("img")
	return p
}

// htmlToText turns an HTML fragment into plain text with collapsed whitespace.
func htmlToText(s string) string {
	s = blockTagRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

func wordCount(text string) int { return len(strings.Fields(text)) }

func readingMinutes(words int) int {
	if words <= 0 {
		return 0
	}
	m := int(math.Round(float64(words) / wordsPerMinute))
	if m < 1 {
		m = 1
	}
	return m
}

// makeSummary returns a short plain-text teaser, cut at a word boundary.
func makeSummary(rawHTML string) string {
	text := htmlToText(rawHTML)
	text = strings.TrimSpace(textFooterRe.ReplaceAllString(text, ""))
	if len(text) <= summaryMaxChars {
		return text
	}
	cut := strings.LastIndex(text[:summaryMaxChars], " ")
	if cut < summaryMaxChars/2 {
		cut = summaryMaxChars
	}
	// Don't split a multi-byte rune.
	for cut > 0 && !isRuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRightFunc(text[:cut], func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	}) + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// cleanTags dedupes feed categories and makes slug-style ones readable
// ("machine-learning" -> "Machine Learning").
func cleanTags(categories []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, c := range categories {
		c = cleanTitle(c)
		if c == "" || strings.EqualFold(c, "uncategorized") || len(c) > 40 {
			continue
		}
		if c == strings.ToLower(c) {
			c = titleCase(strings.NewReplacer("-", " ", "_", " ").Replace(c))
		}
		key := strings.ToLower(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
		if len(out) == maxTags {
			break
		}
	}
	return out
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// sanitizeArticle makes crawled HTML safe and tidy for the app's WebView.
func sanitizeArticle(raw, title string) string {
	s := wpFooterRe.ReplaceAllString(raw, "")
	s = mediumPixelRe.ReplaceAllString(s, "")
	s = articlePolicy.Sanitize(s)
	// The app renders the title itself; drop a duplicate heading at the top.
	if m := leadingH1Re.FindStringSubmatchIndex(s); m != nil {
		if strings.EqualFold(htmlToText(s[m[4]:m[5]]), title) {
			h1Start := strings.LastIndex(strings.ToLower(s[:m[4]]), "<h1")
			s = s[:h1Start] + s[m[1]:]
		}
	}
	return strings.TrimSpace(s)
}

type article struct {
	html    string
	words   int
	byline  string
	excerpt string
}

// fetchArticle downloads a post's page and extracts the main article with a
// Readability port, for feeds that only publish excerpts.
func fetchArticle(ctx context.Context, client *http.Client, link, title string) (article, error) {
	pageURL, err := url.Parse(link)
	if err != nil || (pageURL.Scheme != "http" && pageURL.Scheme != "https") {
		return article{}, fmt.Errorf("bad link %q", link)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return article{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		return article{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return article{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return article{}, errNotHTMLPage
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArticleBytes))
	if err != nil {
		return article{}, err
	}
	// Redirects may have moved us; resolve relative links against the final URL.
	parsed, err := readability.FromReader(bytes.NewReader(body), resp.Request.URL)
	if err != nil {
		return article{}, err
	}
	if parsed.Node == nil {
		return article{}, errNotArticle
	}
	var text, rendered strings.Builder
	if err := parsed.RenderText(&text); err != nil {
		return article{}, err
	}
	words := wordCount(text.String())
	if words < minExtractedWords {
		return article{}, errNotArticle
	}
	if err := parsed.RenderHTML(&rendered); err != nil {
		return article{}, err
	}
	return article{
		html:    sanitizeArticle(rendered.String(), title),
		words:   words,
		byline:  cleanTitle(parsed.Byline()),
		excerpt: makeSummary(parsed.Excerpt()),
	}, nil
}
