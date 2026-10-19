package main

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const localTimeLayout = "2006-01-02T15:04:05" // first_seen / notified_at format

func openDB(p paths) (*sql.DB, error) {
	// WAL lets the API server read while a crawl is writing.
	db, err := sql.Open("sqlite3", p.db+"?_busy_timeout=10000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.db, err)
	}
	db.SetMaxOpenConns(1)
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
	hasNotified, err := hasColumn(db, "posts", "notified_at")
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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS feeds (
			name TEXT PRIMARY KEY,
			url TEXT,
			etag TEXT,
			last_modified TEXT,
			seeded_at TEXT,
			last_success TEXT,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			failure_alerted INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		return err
	}

	// Columns added for the app API. Older DBs get them in place.
	added := []struct{ table, column, decl string }{
		{"posts", "id", "TEXT"},        // stable public id: sha1(guid)[:16]
		{"posts", "sort_time", "TEXT"}, // RFC3339 UTC; published, else first_seen
		{"posts", "summary", "TEXT"},
		{"posts", "author", "TEXT"},
		{"posts", "tags", "TEXT"}, // JSON array
		{"posts", "content_html", "TEXT"},
		{"posts", "word_count", "INTEGER"},
		{"posts", "content_fetched_at", "TEXT"},
		{"posts", "content_attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"posts", "content_error", "TEXT"},
		{"posts", "read_at", "TEXT"},
		{"feeds", "company", "TEXT"},
	}
	for _, a := range added {
		ok, err := hasColumn(db, a.table, a.column)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, a.table, a.column, a.decl)); err != nil {
				return err
			}
		}
	}
	if err := backfillIDs(db); err != nil {
		return fmt.Errorf("backfilling post ids: %w", err)
	}

	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS posts_feed_link ON posts(feed, link);
		CREATE INDEX IF NOT EXISTS posts_pending ON posts(notified_at) WHERE notified_at IS NULL;
		CREATE UNIQUE INDEX IF NOT EXISTS posts_id ON posts(id);
		CREATE INDEX IF NOT EXISTS posts_sort ON posts(sort_time DESC);
		CREATE INDEX IF NOT EXISTS posts_feed_sort ON posts(feed, sort_time DESC);
		INSERT OR IGNORE INTO feeds (name, seeded_at)
			SELECT feed, MIN(first_seen) FROM posts GROUP BY feed`)
	return err
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var ok bool
	err := db.QueryRow(
		`SELECT COUNT(*) > 0 FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&ok)
	return ok, err
}

// Fill id/sort_time for rows written before those columns existed.
func backfillIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT guid, published, first_seen FROM posts WHERE id IS NULL OR sort_time IS NULL`)
	if err != nil {
		return err
	}
	type fix struct{ guid, id, sortTime string }
	var fixes []fix
	for rows.Next() {
		var guid, firstSeen string
		var published sql.NullString
		if err := rows.Scan(&guid, &published, &firstSeen); err != nil {
			rows.Close()
			return err
		}
		fixes = append(fixes, fix{guid, postID(guid), sortTime(parsePublished(published.String), firstSeen)})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(fixes) == 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range fixes {
		if _, err := tx.Exec(`UPDATE posts SET id = ?, sort_time = ? WHERE guid = ?`, f.id, f.sortTime, f.guid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func postID(guid string) string {
	sum := sha1.Sum([]byte(guid))
	return hex.EncodeToString(sum[:8])
}

// Old Python-era rows store raw RFC822 dates; new rows store RFC3339.
var publishedLayouts = []string{
	time.RFC3339,
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2006-01-02",
}

func parsePublished(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, l := range publishedLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return &t
		}
	}
	return nil
}

// sortTime is the instant the app sorts and dates a post by. Future dates
// (scheduled posts, broken feeds) are clamped to when we first saw the post.
func sortTime(published *time.Time, firstSeen string) string {
	seen, err := time.ParseInLocation(localTimeLayout, firstSeen, time.Local)
	if err != nil {
		seen = time.Now()
	}
	if published != nil && !published.After(seen.Add(24*time.Hour)) {
		return published.UTC().Format(time.RFC3339)
	}
	return seen.UTC().Format(time.RFC3339)
}

type feedState struct {
	cache  cacheHeaders
	seeded bool
}

func getFeedState(db *sql.DB, cfg feedCfg) (feedState, error) {
	var dbURL, etag, lm, seededAt sql.NullString
	err := db.QueryRow(
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
		`INSERT OR IGNORE INTO posts (guid, feed, title, link, published, first_seen, notified_at,
			id, sort_time, summary, author, tags, content_html, word_count, content_fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
		var content, fetchedAt sql.NullString
		var words sql.NullInt64
		if it.contentHTML != "" {
			content = sql.NullString{String: it.contentHTML, Valid: true}
			fetchedAt = sql.NullString{String: now, Valid: true}
			words = sql.NullInt64{Int64: int64(it.wordCount), Valid: true}
		}
		tags, _ := json.Marshal(it.tags)
		res, err := insStmt.Exec(it.guid, cfg.name, it.title, it.link, published, now, notifiedAt,
			postID(it.guid), sortTime(it.published, now), nullStr(it.summary), nullStr(it.author),
			string(tags), content, words, fetchedAt)
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
		 ORDER BY sort_time DESC, first_seen DESC`)
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

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ---------------------------------------------------------------------------
// Article content
// ---------------------------------------------------------------------------

type contentJob struct {
	id    string
	link  string
	title string
}

func contentQueue(db *sql.DB, limit int) ([]contentJob, error) {
	rows, err := db.Query(
		`SELECT id, link, title FROM posts
		 WHERE content_html IS NULL AND content_attempts < ? AND link != ''
		 ORDER BY sort_time DESC LIMIT ?`, maxContentAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []contentJob
	for rows.Next() {
		var j contentJob
		if err := rows.Scan(&j.id, &j.link, &j.title); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func saveContent(db *sql.DB, id string, a article) error {
	_, err := db.Exec(
		`UPDATE posts SET content_html = ?, word_count = ?, content_fetched_at = ?, content_error = NULL,
			author = COALESCE(NULLIF(author, ''), ?),
			summary = COALESCE(NULLIF(summary, ''), ?)
		 WHERE id = ?`,
		a.html, a.words, time.Now().Format(localTimeLayout), nullStr(a.byline), nullStr(a.excerpt), id)
	return err
}

func saveContentFailure(db *sql.DB, id string, cause error) error {
	_, err := db.Exec(
		`UPDATE posts SET content_attempts = content_attempts + 1, content_error = ? WHERE id = ?`,
		cause.Error(), id)
	return err
}

// ---------------------------------------------------------------------------
// API queries
// ---------------------------------------------------------------------------

type apiPost struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Company        string   `json:"company"`
	Author         *string  `json:"author"`
	PublishedAt    string   `json:"publishedAt"` // YYYY-MM-DD
	Summary        string   `json:"summary"`
	Tags           []string `json:"tags"`
	ReadingMinutes *int     `json:"readingMinutes"` // null until the article body is known
	URL            string   `json:"url"`
	IsRead         bool     `json:"isRead"`
}

type postFilter struct {
	company string
	query   string
	unread  bool
	limit   int
	offset  int
}

const companyExpr = `COALESCE(NULLIF(f.company, ''), p.feed)`

const postColumns = `p.id, p.title, ` + companyExpr + `, p.author, p.sort_time, p.summary, p.tags,
	p.word_count, p.link, p.read_at IS NOT NULL`

func scanPost(sc interface{ Scan(...any) error }, extra ...any) (apiPost, error) {
	var (
		p         apiPost
		author    sql.NullString
		sortT     sql.NullString
		summary   sql.NullString
		tags      sql.NullString
		wordCount sql.NullInt64
	)
	dest := append([]any{&p.ID, &p.Title, &p.Company, &author, &sortT, &summary, &tags,
		&wordCount, &p.URL, &p.IsRead}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return apiPost{}, err
	}
	if author.Valid && author.String != "" {
		p.Author = &author.String
	}
	if t, err := time.Parse(time.RFC3339, sortT.String); err == nil {
		p.PublishedAt = t.Local().Format("2006-01-02")
	}
	p.Summary = summary.String
	p.Tags = []string{}
	if tags.Valid {
		_ = json.Unmarshal([]byte(tags.String), &p.Tags)
	}
	if wordCount.Valid {
		m := readingMinutes(int(wordCount.Int64))
		p.ReadingMinutes = &m
	}
	return p, nil
}

func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// listPosts returns one page of posts, newest first, plus the total match count.
func listPosts(db *sql.DB, f postFilter) ([]apiPost, int, error) {
	var where []string
	var args []any
	if f.company != "" {
		where = append(where, companyExpr+` = ?`)
		args = append(args, f.company)
	}
	if q := strings.TrimSpace(f.query); q != "" {
		like := likePattern(q)
		where = append(where, `(p.title LIKE ? ESCAPE '\' OR p.summary LIKE ? ESCAPE '\'
			OR p.tags LIKE ? ESCAPE '\' OR `+companyExpr+` LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	if f.unread {
		where = append(where, `p.read_at IS NULL`)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	from := `FROM posts p LEFT JOIN feeds f ON f.name = p.feed ` + whereSQL

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) `+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Query(`SELECT `+postColumns+` `+from+` ORDER BY p.sort_time DESC, p.id LIMIT ? OFFSET ?`,
		append(args, f.limit, f.offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	posts := []apiPost{}
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, 0, err
		}
		posts = append(posts, p)
	}
	return posts, total, rows.Err()
}

type storedPost struct {
	post        apiPost
	contentHTML sql.NullString
	attempts    int
}

func getPost(db *sql.DB, id string) (storedPost, error) {
	var sp storedPost
	row := db.QueryRow(`SELECT `+postColumns+`, p.content_html, p.content_attempts
		FROM posts p LEFT JOIN feeds f ON f.name = p.feed WHERE p.id = ?`, id)
	p, err := scanPost(row, &sp.contentHTML, &sp.attempts)
	sp.post = p
	return sp, err
}

type companyCount struct {
	Name      string `json:"name"`
	PostCount int    `json:"postCount"`
}

func listCompanies(db *sql.DB) ([]companyCount, error) {
	rows, err := db.Query(`SELECT ` + companyExpr + ` AS company, COUNT(*) AS n
		FROM posts p LEFT JOIN feeds f ON f.name = p.feed
		GROUP BY company ORDER BY n DESC, company`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []companyCount{}
	for rows.Next() {
		var c companyCount
		if err := rows.Scan(&c.Name, &c.PostCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// setRead marks a post read/unread. Returns false if the id doesn't exist.
func setRead(db *sql.DB, id string, read bool) (bool, error) {
	var res sql.Result
	var err error
	if read {
		res, err = db.Exec(`UPDATE posts SET read_at = COALESCE(read_at, ?) WHERE id = ?`,
			time.Now().Format(localTimeLayout), id)
	} else {
		res, err = db.Exec(`UPDATE posts SET read_at = NULL WHERE id = ?`, id)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
