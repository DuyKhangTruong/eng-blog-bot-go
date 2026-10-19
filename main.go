// Command eng-blog-bot is a daily engineering-blog digest crawler.
//
// It fetches RSS/Atom feeds of engineering blogs in parallel, tracks seen
// posts in SQLite, writes a daily markdown digest, and notifies the user via
// the `musegadget` CLI.
//
// Drop-in compatible with the Python version's seen.db: the first run
// migrates the schema in place and does not re-announce old posts.
//
// Usage:
//
//	eng-blog-bot [--dry-run]
//
// Environment:
//
//	ENG_BLOG_BOT_DIR  data directory (default ~/Development/eng-blog-bot)
//	MUSEGADGET_BIN    full path to musegadget (default "musegadget" on PATH)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Used when <base>/feeds.txt doesn't exist.
var defaultFeeds = []feedCfg{
	{"Cloudflare Blog", "https://blog.cloudflare.com/rss/"},
	{"Netflix TechBlog", "https://medium.com/feed/netflix-techblog"},
	{"Airbnb Engineering", "https://medium.com/feed/airbnb-engineering"},
	{"Google Developers Blog", "https://developers.googleblog.com/rss/"},
	{"AWS Architecture Blog", "https://aws.amazon.com/blogs/architecture/feed/"},
	{"Meta Engineering", "https://engineering.fb.com/feed/"},
}

const userAgent = "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36 eng-blog-bot/1.0"

const (
	maxPerFeedInDigest  = 8
	highlightsPerFeed   = 2
	maxHighlights       = 8
	httpTimeout         = 30 * time.Second
	maxFeedBytes        = 10 * 1024 * 1024
	museTimeout         = 30 * time.Second
	failureAlertAfter   = 3 // alert once a feed has failed this many runs in a row
	logMaxBytes         = 1024 * 1024
)

type feedCfg struct {
	name string
	url  string
}

// A feed seen for the first time in this run, with its back-catalog size.
type seedInfo struct {
	name  string
	fresh int
}

type paths struct {
	db        string
	digests   string
	log       string
	feedsFile string
}

func resolvePaths() (paths, error) {
	base := os.Getenv("ENG_BLOG_BOT_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return paths{}, fmt.Errorf("ENG_BLOG_BOT_DIR not set and HOME unavailable")
		}
		base = filepath.Join(home, "Development", "eng-blog-bot")
	}
	return paths{
		db:        filepath.Join(base, "seen.db"),
		digests:   filepath.Join(base, "digests"),
		log:       filepath.Join(base, "crawl.log"),
		feedsFile: filepath.Join(base, "feeds.txt"),
	}, nil
}

// feeds.txt format: one "Name | URL" per line, "#" starts a comment.
// Lets you add or remove blogs without recompiling.
func loadFeeds(p paths, log *logger) ([]feedCfg, error) {
	data, err := os.ReadFile(p.feedsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultFeeds, nil
		}
		return nil, fmt.Errorf("reading feeds.txt: %w", err)
	}
	var feeds []feedCfg
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, u, ok := strings.Cut(line, "|")
		name, u = strings.TrimSpace(name), strings.TrimSpace(u)
		if !ok || name == "" || u == "" {
			log.log(fmt.Sprintf("feeds.txt line %d: expected `Name | URL`, skipped", i+1))
			continue
		}
		feeds = append(feeds, feedCfg{name, u})
	}
	if len(feeds) == 0 {
		return nil, fmt.Errorf("%s has no usable feeds", p.feedsFile)
	}
	return feeds, nil
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

type logger struct {
	path string
}

func newLogger(path string) *logger {
	if st, err := os.Stat(path); err == nil && st.Size() > logMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	return &logger{path: path}
}

func (l *logger) log(msg string) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02 15:04:05"), msg)
	fmt.Println(line)
	if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, line)
		_ = f.Close()
	}
}

// ---------------------------------------------------------------------------
// Fetching
// ---------------------------------------------------------------------------

type item struct {
	guid      string
	title     string
	link      string
	published *time.Time
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
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
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
		items = append(items, item{guid: guid, title: title, link: link, published: pub})
	}
	return outcome{
		items: items,
		cache: cacheHeaders{
			etag:         resp.Header.Get("ETag"),
			lastModified: resp.Header.Get("Last-Modified"),
		},
	}
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

func openDB(p paths) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", p.db+"?cache=shared")
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.db, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 10000"); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS posts (
		guid TEXT PRIMARY KEY,
		feed TEXT NOT NULL,
		title TEXT NOT NULL,
		link TEXT NOT NULL,
		published TEXT,
		first_seen TEXT NOT NULL,
		notified_at TEXT
	)`); err != nil {
		return err
	}
	var hasNotified bool
	err := db.QueryRow(
		`SELECT COUNT(*) > 0 FROM pragma_table_info('posts') WHERE name = 'notified_at'`,
	).Scan(&hasNotified)
	if err != nil {
		return err
	}
	if !hasNotified {
		// Upgrading a Python-era DB: everything in it was already handled.
		if _, err := db.Exec(`ALTER TABLE posts ADD COLUMN notified_at TEXT`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE posts SET notified_at = first_seen`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS posts_feed_link ON posts(feed, link);
		CREATE INDEX IF NOT EXISTS posts_pending ON posts(notified_at) WHERE notified_at IS NULL;
		CREATE TABLE IF NOT EXISTS feeds (
			name TEXT PRIMARY KEY,
			url TEXT,
			etag TEXT,
			last_modified TEXT,
			seeded_at TEXT,
			last_success TEXT,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			failure_alerted INTEGER NOT NULL DEFAULT 0
		);
		INSERT OR IGNORE INTO feeds (name, seeded_at)
			SELECT feed, MIN(first_seen) FROM posts GROUP BY feed`)
	return err
}

type feedState struct {
	cache  cacheHeaders
	seeded bool
}

func getFeedState(tx *sql.Tx, cfg feedCfg) (feedState, error) {
	var (
		dbURL, etag, lm sql.NullString
		seededAt        sql.NullString
	)
	err := tx.QueryRow(
		`SELECT url, etag, last_modified, seeded_at FROM feeds WHERE name = ?`, cfg.name,
	).Scan(&dbURL, &etag, &lm, &seededAt)
	if err == sql.ErrNoRows {
		return feedState{}, nil
	}
	if err != nil {
		return feedState{}, err
	}
	st := feedState{seeded: seededAt.Valid}
	// Only reuse cache validators if they belong to the same URL.
	if dbURL.Valid && dbURL.String == cfg.url {
		st.cache = cacheHeaders{etag: etag.String, lastModified: lm.String}
	}
	return st, nil
}

// Insert unseen items; returns how many were new. A post counts as seen if
// its guid OR its link is already known for this feed, which survives feeds
// that change their guid scheme (a classic cause of digest floods).
func storeItems(tx *sql.Tx, cfg feedCfg, items []item, now string, markNotified bool) (int, error) {
	seenStmt, err := tx.Prepare(
		`SELECT 1 FROM posts WHERE guid = ? OR (feed = ? AND link = ? AND link != '')`)
	if err != nil {
		return 0, err
	}
	defer seenStmt.Close()
	insStmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO posts (guid, feed, title, link, published, first_seen, notified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insStmt.Close()

	fresh := 0
	for _, it := range items {
		var dummy int
		if err := seenStmt.QueryRow(it.guid, cfg.name, it.link).Scan(&dummy); err == nil {
			continue
		} else if err != sql.ErrNoRows {
			return 0, err
		}
		var published, notifiedAt sql.NullString
		if it.published != nil {
			published = sql.NullString{String: it.published.UTC().Format(time.RFC3339), Valid: true}
		}
		if markNotified {
			notifiedAt = sql.NullString{String: now, Valid: true}
		}
		res, err := insStmt.Exec(it.guid, cfg.name, it.title, it.link, published, now, notifiedAt)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fresh++
		}
	}
	return fresh, nil
}

type pending struct {
	feed      string
	title     string
	link      string
	published sql.NullString
}

func pendingPosts(tx *sql.Tx) ([]pending, error) {
	rows, err := tx.Query(
		`SELECT feed, title, link, published FROM posts
		 WHERE notified_at IS NULL
		 ORDER BY published IS NULL, published DESC, first_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.feed, &p.title, &p.link, &p.published); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "[", "\\[")
	return strings.ReplaceAll(s, "]", "\\]")
}

// Old Python-era rows store raw RFC822 dates; new rows store RFC3339.
func shortDate(published sql.NullString) string {
	if !published.Valid || published.String == "" {
		return ""
	}
	layouts := []string{
		time.RFC3339,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, strings.TrimSpace(published.String)); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// Group pending posts by feed, in config order, then any no-longer-configured feeds.
type feedGroup struct {
	name  string
	posts []pending
}

func groupByFeed(feeds []feedCfg, posts []pending) []feedGroup {
	byFeed := map[string][]pending{}
	for _, p := range posts {
		byFeed[p.feed] = append(byFeed[p.feed], p)
	}
	var groups []feedGroup
	for _, f := range feeds {
		if v, ok := byFeed[f.name]; ok {
			groups = append(groups, feedGroup{f.name, v})
			delete(byFeed, f.name)
		}
	}
	var rest []string
	for name := range byFeed {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		groups = append(groups, feedGroup{name, byFeed[name]})
	}
	return groups
}

func renderDigest(today string, groups []feedGroup, failing []string, appendMode bool) string {
	var s strings.Builder
	if appendMode {
		fmt.Fprintf(&s, "\n---\n\n_Later run at %s_\n\n", time.Now().Format("15:04"))
	} else {
		fmt.Fprintf(&s, "# Engineering digest — %s\n\n", today)
	}
	for _, g := range groups {
		fmt.Fprintf(&s, "## %s (%d new)\n", g.name, len(g.posts))
		for i, p := range g.posts {
			if i >= maxPerFeedInDigest {
				break
			}
			fmt.Fprintf(&s, "- [%s](<%s>)", mdEscape(p.title), p.link)
			if d := shortDate(p.published); d != "" {
				fmt.Fprintf(&s, " — %s", d)
			}
			s.WriteString("\n")
		}
		if len(g.posts) > maxPerFeedInDigest {
			fmt.Fprintf(&s, "- …and %d more\n", len(g.posts)-maxPerFeedInDigest)
		}
		s.WriteString("\n")
	}
	if len(failing) > 0 {
		s.WriteString("## Feed problems\n")
		for _, f := range failing {
			fmt.Fprintf(&s, "- %s\n", f)
		}
		s.WriteString("\n")
	}
	return s.String()
}

func sendToMuse(text string, log *logger) bool {
	bin := os.Getenv("MUSEGADGET_BIN")
	if bin == "" {
		bin = "musegadget"
	}
	ctx, cancel := context.WithTimeout(context.Background(), museTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "send-user-msg", text)
	cmd.Stdout = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.log(fmt.Sprintf("send-user-msg failed: timed out after %ds", int(museTimeout.Seconds())))
		} else {
			log.log(fmt.Sprintf("send-user-msg failed: %v: %s", err, strings.TrimSpace(stderr.String())))
		}
		return false
	}
	log.log("sent message via musegadget")
	return true
}

func hostname() string {
	if b, err := os.ReadFile("/proc/sys/kernel/hostname"); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	return "khangpi"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	os.Exit(run())
}

func run() int {
	var dryRun, showHelp bool
	fs := flag.NewFlagSet("eng-blog-bot", flag.ContinueOnError)
	fs.BoolVar(&dryRun, "dry-run", false, "fetch and print the digest/message, but don't save anything or send")
	fs.BoolVar(&showHelp, "h", false, "show help")
	parseErr := fs.Parse(os.Args[1:])
	if showHelp || parseErr == flag.ErrHelp {
		fmt.Println("eng-blog-bot [--dry-run]\n\n  --dry-run   fetch and print the digest/message, but don't save anything or send\n\nEnv: ENG_BLOG_BOT_DIR (default ~/Development/eng-blog-bot), MUSEGADGET_BIN (default musegadget)")
		return 0
	}
	if parseErr != nil {
		return 2
	}

	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(p.digests, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: creating %s: %v\n", p.digests, err)
		return 1
	}
	log := newLogger(p.log)

	feeds, err := loadFeeds(p, log)
	if err != nil {
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	}
	db, err := openDB(p)
	if err != nil {
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	}
	defer db.Close()

	// Fetch every feed concurrently: the run is network-bound, so this cuts
	// wall time to roughly the slowest single feed.
	client := &http.Client{Timeout: httpTimeout}
	outcomes := make([]outcome, len(feeds))
	states := make([]feedState, len(feeds))
	var wg sync.WaitGroup
	for i, cfg := range feeds {
		wg.Add(1)
		go func(i int, cfg feedCfg) {
			defer wg.Done()
			// Read this feed's cache headers outside the write transaction.
			var st feedState
			_ = func() error {
				tx, err := db.Begin()
				if err != nil {
					return err
				}
				defer tx.Rollback()
				s, err := getFeedState(tx, cfg)
				if err != nil {
					return err
				}
				st = s
				return tx.Commit()
			}()
			states[i] = st
			outcomes[i] = fetchFeed(client, cfg, st.cache)
		}(i, cfg)
	}
	wg.Wait()

	var anySeeded bool
	_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM feeds WHERE seeded_at IS NOT NULL)`).Scan(&anySeeded)

	now := time.Now().Format("2006-01-02T15:04:05")
	today := time.Now().Format("2006-01-02")
	tx, err := db.Begin()
	if err != nil {
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var newlySeeded []seedInfo
	var failing []string
	var alertFailing []string
	var alertedFeeds []string
	failures := 0

	for i, cfg := range feeds {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO feeds (name) VALUES (?)`, cfg.name); err != nil {
			log.log(fmt.Sprintf("fatal: %v", err))
			return 1
		}
		oc := outcomes[i]
		st := states[i]
		switch {
		case oc.failed != "":
			failures++
			log.log(fmt.Sprintf("%s: fetch failed: %s", cfg.name, oc.failed))
			var n int
			var alerted bool
			err := tx.QueryRow(
				`UPDATE feeds SET consecutive_failures = consecutive_failures + 1,
					last_error = ? WHERE name = ? RETURNING consecutive_failures, failure_alerted`,
				oc.failed, cfg.name).Scan(&n, &alerted)
			if err != nil {
				log.log(fmt.Sprintf("fatal: %v", err))
				return 1
			}
			if n >= failureAlertAfter {
				msg := fmt.Sprintf("%s has failed %d runs in a row (%s)", cfg.name, n, oc.failed)
				if !alerted {
					alertFailing = append(alertFailing, msg)
					alertedFeeds = append(alertedFeeds, cfg.name)
				}
				failing = append(failing, msg)
			}
		case oc.notModified:
			log.log(fmt.Sprintf("%s: not modified", cfg.name))
			if _, err := tx.Exec(
				`UPDATE feeds SET last_success = ?, consecutive_failures = 0,
					last_error = NULL, failure_alerted = 0 WHERE name = ?`, now, cfg.name); err != nil {
				log.log(fmt.Sprintf("fatal: %v", err))
				return 1
			}
		default:
			// A feed's first successful fetch just indexes its back catalog.
			fresh, err := storeItems(tx, cfg, oc.items, now, !st.seeded)
			if err != nil {
				log.log(fmt.Sprintf("fatal: %v", err))
				return 1
			}
			if !st.seeded {
				newlySeeded = append(newlySeeded, seedInfo{cfg.name, fresh})
			}
			log.log(fmt.Sprintf("%s: %d entries, %d new", cfg.name, len(oc.items), fresh))
			if _, err := tx.Exec(
				`UPDATE feeds SET url = ?, etag = ?, last_modified = ?, last_success = ?,
					consecutive_failures = 0, last_error = NULL, failure_alerted = 0,
					seeded_at = COALESCE(seeded_at, ?) WHERE name = ?`,
				cfg.url, nullStr(oc.cache.etag), nullStr(oc.cache.lastModified),
				now, now, cfg.name); err != nil {
				log.log(fmt.Sprintf("fatal: %v", err))
				return 1
			}
		}
	}

	// Everything not yet announced, including posts left over from a run
	// where musegadget failed.
	pendingList, err := pendingPosts(tx)
	if err != nil {
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	}
	groups := groupByFeed(feeds, pendingList)

	var message []string
	if len(newlySeeded) > 0 {
		indexed := 0
		for _, ns := range newlySeeded {
			indexed += ns.fresh
		}
		if !anySeeded {
			var names []string
			for _, ns := range newlySeeded {
				names = append(names, ns.name)
			}
			message = append(message, fmt.Sprintf(
				"Engineering blog bot initialized on %s. Indexed %s across %s (%s). "+
					"You'll get a daily digest of new posts from now on.",
				hostname(), plural(indexed, "existing post"), plural(len(names), "feed"),
				strings.Join(names, ", ")))
		} else {
			var names []string
			for _, ns := range newlySeeded {
				names = append(names, fmt.Sprintf("%s (%s indexed)", ns.name, plural(ns.fresh, "existing post")))
			}
			message = append(message, fmt.Sprintf("Now following: %s.", strings.Join(names, ", ")))
		}
	}

	digestPath := filepath.Join(p.digests, today+".md")
	if len(pendingList) > 0 {
		var highlights []string
		for _, g := range groups {
			for i, post := range g.posts {
				if i >= highlightsPerFeed {
					break
				}
				highlights = append(highlights, fmt.Sprintf("- %s: %s", g.name, post.title))
				if len(highlights) >= maxHighlights {
					break
				}
			}
			if len(highlights) >= maxHighlights {
				break
			}
		}
		var active []string
		for _, g := range groups {
			active = append(active, g.name)
		}
		message = append(message, fmt.Sprintf(
			"Morning engineering digest (%s): %s across %s.\n%s\nFull digest: %s",
			today, plural(len(pendingList), "new post"), plural(len(active), "blog"),
			strings.Join(highlights, "\n"), digestPath))
	}
	for _, a := range alertFailing {
		message = append(message, "⚠ "+a)
	}

	if len(message) == 0 {
		log.log("no new posts today")
		if !dryRun {
			if err := tx.Commit(); err != nil {
				log.log(fmt.Sprintf("fatal: %v", err))
				return 1
			}
			committed = true
		}
		return exitCode(failures, len(feeds))
	}
	fullMessage := strings.Join(message, "\n\n")

	if dryRun {
		if len(pendingList) > 0 {
			fmt.Printf("\n--- digest (%s) ---\n%s", digestPath, renderDigest(today, groups, failing, false))
		}
		fmt.Printf("--- message ---\n%s\n\n(dry run: nothing saved or sent)\n", fullMessage)
		return exitCode(failures, len(feeds)) // tx rolled back by defer
	}

	// Remember the digest file's previous length so a failed send can undo
	// the append; the retry next run then writes these posts exactly once.
	var digestUndo *int64
	if len(pendingList) > 0 {
		var prevLen *int64
		if st, err := os.Stat(digestPath); err == nil {
			l := st.Size()
			prevLen = &l
		}
		f, err := os.OpenFile(digestPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.log(fmt.Sprintf("fatal: %v", err))
			return 1
		}
		_, werr := f.WriteString(renderDigest(today, groups, failing, prevLen != nil))
		cerr := f.Close()
		if werr != nil || cerr != nil {
			log.log(fmt.Sprintf("fatal writing digest: %v %v", werr, cerr))
			return 1
		}
		digestUndo = prevLen
		log.log(fmt.Sprintf("wrote %s", digestPath))
	}

	if sendToMuse(fullMessage, log) {
		if _, err := tx.Exec(`UPDATE posts SET notified_at = ? WHERE notified_at IS NULL`, now); err != nil {
			log.log(fmt.Sprintf("fatal: %v", err))
			return 1
		}
		// Mark failure alerts as sent (only after a successful send).
		for _, name := range alertedFeeds {
			if _, err := tx.Exec(`UPDATE feeds SET failure_alerted = 1 WHERE name = ?`, name); err != nil {
				log.log(fmt.Sprintf("fatal: %v", err))
				return 1
			}
		}
	} else {
		switch {
		case digestUndo == nil:
			_ = os.Remove(digestPath)
		default:
			if f, err := os.OpenFile(digestPath, os.O_WRONLY, 0o644); err == nil {
				_ = f.Truncate(*digestUndo)
				_ = f.Close()
			}
		}
		if len(pendingList) > 0 {
			log.log(fmt.Sprintf("%s kept pending; will retry next run", plural(len(pendingList), "post")))
		}
	}
	if err := tx.Commit(); err != nil {
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	}
	committed = true
	return exitCode(failures, len(feeds))
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// Non-zero only when every feed failed (likely no network), so the scheduler
// marks the run as failed without crying wolf over one flaky blog.
func exitCode(failures, total int) int {
	if total > 0 && failures == total {
		return 1
	}
	return 0
}
