package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(paths{db: filepath.Join(t.TempDir(), "seen.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seed(t *testing.T, db *sql.DB, cfg feedCfg, items []item) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO feeds (name, company) VALUES (?, ?)`, cfg.name, cfg.company); err != nil {
		t.Fatal(err)
	}
	if _, err := storeItems(tx, cfg, items, time.Now().Format(localTimeLayout), true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func day(n int) *time.Time {
	t := time.Now().AddDate(0, 0, -n)
	return &t
}

func getJSON(t *testing.T, srv *httptest.Server, path string, out any) int {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

type listResp struct {
	Posts      []apiPost `json:"posts"`
	Total      int       `json:"total"`
	NextOffset *int      `json:"nextOffset"`
}

func TestAPI(t *testing.T) {
	// A fake blog page for the on-demand article download.
	article := "<html><head><title>Excerpt Only</title></head><body><nav>menu</nav><article><h1>Excerpt Only</h1>" +
		strings.Repeat("<p>Distributed systems are fun when the retries are idempotent and the queues drain.</p>", 30) +
		"</article></body></html>"
	blog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, article)
	}))
	defer blog.Close()

	db := testDB(t)
	full := strings.Repeat("<p>Lots of words about Kafka consumers and backpressure.</p>", 40)
	seed(t, db, feedCfg{"Netflix TechBlog", "https://n/feed", "Netflix"}, []item{
		{guid: "n1", title: "Kafka at scale", link: "https://n/1", published: day(1), summary: "Kafka stuff",
			tags: []string{"Kafka"}, contentHTML: full, wordCount: 460, author: "Jane"},
		{guid: "n2", title: "Older post", link: "https://n/2", published: day(10), summary: "Old", tags: []string{}},
	})
	seed(t, db, feedCfg{"Stripe Blog", "https://s/feed", "Stripe"}, []item{
		{guid: "s1", title: "Excerpt Only", link: blog.URL + "/post", published: day(2), summary: "Teaser", tags: []string{"API"}},
	})

	a := &api{db: db, log: &logger{}, client: http.DefaultClient}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	var list listResp
	getJSON(t, srv, "/api/posts?limit=2", &list)
	if list.Total != 3 || len(list.Posts) != 2 || list.NextOffset == nil || *list.NextOffset != 2 {
		t.Fatalf("page 1 = %+v", list)
	}
	if list.Posts[0].Title != "Kafka at scale" || list.Posts[1].Company != "Stripe" {
		t.Errorf("wrong order/company: %+v", list.Posts)
	}
	if list.Posts[0].ReadingMinutes == nil || *list.Posts[0].ReadingMinutes != 2 {
		t.Errorf("reading minutes = %v", list.Posts[0].ReadingMinutes)
	}
	if list.Posts[1].ReadingMinutes != nil {
		t.Errorf("unknown length should be null, got %v", *list.Posts[1].ReadingMinutes)
	}

	getJSON(t, srv, "/api/posts?offset=2", &list)
	if len(list.Posts) != 1 || list.NextOffset != nil {
		t.Errorf("last page = %+v", list)
	}

	getJSON(t, srv, "/api/posts?company=Netflix", &list)
	if list.Total != 2 {
		t.Errorf("company filter total = %d", list.Total)
	}
	getJSON(t, srv, "/api/posts?q=kafka", &list)
	if list.Total != 1 || list.Posts[0].ID != postID("n1") {
		t.Errorf("search = %+v", list)
	}
	getJSON(t, srv, "/api/posts?q=100%25", &list) // literal % must not match everything
	if list.Total != 0 {
		t.Errorf("LIKE wildcard not escaped: %d results", list.Total)
	}

	var companies []companyCount
	getJSON(t, srv, "/api/companies", &companies)
	if len(companies) != 2 || companies[0] != (companyCount{"Netflix", 2}) {
		t.Errorf("companies = %+v", companies)
	}

	// Stored content.
	var detail struct {
		Post          apiPost `json:"post"`
		ContentHTML   string  `json:"contentHtml"`
		IsFullArticle bool    `json:"isFullArticle"`
	}
	if code := getJSON(t, srv, "/api/posts/"+postID("n1"), &detail); code != 200 {
		t.Fatalf("detail status %d", code)
	}
	if !detail.IsFullArticle || !strings.Contains(detail.ContentHTML, "Kafka consumers") || *detail.Post.Author != "Jane" {
		t.Errorf("detail = %+v", detail)
	}

	// Content downloaded on demand, then cached.
	getJSON(t, srv, "/api/posts/"+postID("s1"), &detail)
	if !detail.IsFullArticle || !strings.Contains(detail.ContentHTML, "idempotent") {
		t.Fatalf("on-demand fetch failed: %+v", detail)
	}
	if strings.Contains(detail.ContentHTML, "<h1") || detail.Post.ReadingMinutes == nil {
		t.Errorf("duplicate title or missing reading time: %+v", detail)
	}
	blog.Close()
	getJSON(t, srv, "/api/posts/"+postID("s1"), &detail)
	if !detail.IsFullArticle {
		t.Error("content wasn't cached in the DB")
	}

	if code := getJSON(t, srv, "/api/posts/nope", nil); code != 404 {
		t.Errorf("missing post status %d", code)
	}

	// Read state round trip.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/posts/"+postID("n2")+"/read", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("mark read: %v %v", err, resp)
	}
	getJSON(t, srv, "/api/posts?unread=1", &list)
	if list.Total != 2 {
		t.Errorf("unread filter total = %d", list.Total)
	}
}

func TestAPIToken(t *testing.T) {
	a := &api{db: testDB(t), log: &logger{}, token: "s3cret"}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	if code := getJSON(t, srv, "/api/health", nil); code != 401 {
		t.Errorf("no token: status %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("with token: %v %v", err, resp.StatusCode)
	}
}

// A seen.db written by the old Python bot must upgrade without re-announcing anything.
func TestMigratesPythonEraDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen.db")
	old, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE posts (guid TEXT PRIMARY KEY, feed TEXT NOT NULL, title TEXT NOT NULL,
		link TEXT NOT NULL, published TEXT, first_seen TEXT NOT NULL);
		INSERT INTO posts VALUES ('g1', 'Cloudflare Blog', 'Old', 'https://c/1', 'Tue, 06 Oct 2026 17:01:04 GMT', '2026-10-07T08:00:00')`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}

	db, err := openDB(paths{db: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id, sortT string
	var notified sql.NullString
	if err := db.QueryRow(`SELECT id, sort_time, notified_at FROM posts`).Scan(&id, &sortT, &notified); err != nil {
		t.Fatal(err)
	}
	if id != postID("g1") || sortT != "2026-10-06T17:01:04Z" || !notified.Valid {
		t.Errorf("id=%s sort=%s notified=%v", id, sortT, notified)
	}
	posts, _, err := listPosts(db, postFilter{limit: 10})
	if err != nil || len(posts) != 1 || posts[0].Company != "Cloudflare Blog" {
		t.Errorf("listPosts on migrated DB: %v %+v", err, posts)
	}
}
