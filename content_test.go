package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMakeSummary(t *testing.T) {
	got := makeSummary(`<p>Hello <b>world</b> &amp; friends.</p><p>The post <a href="x">Hi</a> appeared first on Some Blog.</p>`)
	if got != "Hello world & friends." {
		t.Errorf("footer not stripped: %q", got)
	}
	long := strings.Repeat("word ", 100)
	s := makeSummary(long)
	if len(s) > summaryMaxChars+len("…") || !strings.HasSuffix(s, "…") {
		t.Errorf("not truncated at a word boundary: %q", s)
	}
	if strings.Contains(s, "wor…") {
		t.Errorf("cut mid-word: %q", s)
	}
}

func TestCleanTags(t *testing.T) {
	got := cleanTags([]string{"machine-learning", "Kotlin", "kotlin", "Uncategorized", "", "AWS"})
	want := []string{"Machine Learning", "Kotlin", "AWS"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cleanTags = %v, want %v", got, want)
	}
	if cleanTags(nil) == nil {
		t.Error("cleanTags(nil) should be an empty slice so it marshals as []")
	}
}

func TestSanitizeArticle(t *testing.T) {
	in := `<h1>My Title</h1><p onclick="x()">Body <script>alert(1)</script></p>` +
		`<img src="https://medium.com/_/stat?event=post.clientViewed">` +
		`<pre><code>fmt.Println("hi")</code></pre>`
	out := sanitizeArticle(in, "My Title")
	for _, bad := range []string{"<h1", "onclick", "<script", "medium.com/_/stat"} {
		if strings.Contains(out, bad) {
			t.Errorf("sanitized output still contains %q: %s", bad, out)
		}
	}
	if !strings.Contains(out, "<pre><code>") {
		t.Errorf("code block lost: %s", out)
	}
	// A heading that isn't the title stays.
	if out := sanitizeArticle(`<h1>Intro</h1><p>x</p>`, "My Title"); !strings.Contains(out, "<h1>Intro</h1>") {
		t.Errorf("unrelated h1 removed: %s", out)
	}
}

func TestReadingMinutes(t *testing.T) {
	cases := map[int]int{0: 0, 10: 1, 230: 1, 1150: 5}
	for words, want := range cases {
		if got := readingMinutes(words); got != want {
			t.Errorf("readingMinutes(%d) = %d, want %d", words, got, want)
		}
	}
}

func TestParseFeeds(t *testing.T) {
	feeds, problems := parseFeeds(`
# comment
Netflix TechBlog | https://medium.com/feed/netflix-techblog | Netflix
Old Style | https://example.com/feed
broken line
Old Style | https://example.com/other
`)
	if len(feeds) != 2 || len(problems) != 2 {
		t.Fatalf("feeds=%v problems=%v", feeds, problems)
	}
	if feeds[0].company != "Netflix" || feeds[1].company != "Old Style" {
		t.Errorf("companies = %q, %q", feeds[0].company, feeds[1].company)
	}
}

func TestSortTimeClampsFutureDates(t *testing.T) {
	firstSeen := "2026-10-18T09:00:00"
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	seen, _ := time.ParseInLocation(localTimeLayout, firstSeen, time.Local)
	if got := sortTime(&future, firstSeen); got != seen.UTC().Format(time.RFC3339) {
		t.Errorf("future date not clamped: %s", got)
	}
	past := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if got := sortTime(&past, firstSeen); got != "2026-10-01T12:00:00Z" {
		t.Errorf("past date changed: %s", got)
	}
}

func TestStripControlChars(t *testing.T) {
	in := []byte("<title>a\x1db\tc\xe9</title>\n")
	if got := string(stripControlChars(in)); got != "<title>ab\tc\xe9</title>\n" {
		t.Errorf("got %q", got)
	}
}

// feeds.txt is the copy you edit on the Pi; it should start out matching the built-in list.
func TestFeedsFileMatchesDefaults(t *testing.T) {
	data, err := os.ReadFile("feeds.txt")
	if err != nil {
		t.Fatal(err)
	}
	feeds, problems := parseFeeds(string(data))
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	if !reflect.DeepEqual(feeds, defaultFeeds) {
		t.Errorf("feeds.txt (%d feeds) differs from defaultFeeds (%d)", len(feeds), len(defaultFeeds))
	}
}
