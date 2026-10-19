package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPageSize  = 30
	maxPageSize      = 100
	onDemandFetchMax = 25 * time.Second // opening a post whose article isn't downloaded yet
)

// HTTP API consumed by the Android app:
//
//	GET    /api/health                      {"ok", "posts", "lastCrawl"}
//	GET    /api/posts?limit&offset&company&q&unread=1
//	                                        {"posts": [...], "total", "nextOffset"}
//	GET    /api/posts/{id}                  {"post", "contentHtml", "isFullArticle"}
//	PUT    /api/posts/{id}/read             mark read   (204)
//	DELETE /api/posts/{id}/read             mark unread (204)
//	GET    /api/companies                   [{"name", "postCount"}]
func runServe(args []string) int {
	fs := flag.NewFlagSet("eng-blog-bot serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			printUsage()
			return 0
		}
		return 2
	}
	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(p.digests, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: creating data dir: %v\n", err)
		return 1
	}
	log := &logger{} // stdout only; journald keeps it
	db, err := openDB(p)
	if err != nil {
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	}
	defer db.Close()

	a := &api{
		db:     db,
		log:    log,
		token:  os.Getenv("ENG_BLOG_API_TOKEN"),
		client: &http.Client{Timeout: onDemandFetchMax},
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      onDemandFetchMax + 15*time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	auth := "no auth"
	if a.token != "" {
		auth = "bearer token required"
	}
	log.log(fmt.Sprintf("serving API on %s (%s), db %s", *addr, auth, p.db))

	select {
	case err := <-errCh:
		log.log(fmt.Sprintf("fatal: %v", err))
		return 1
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		log.log("stopped")
		return 0
	}
}

type api struct {
	db     *sql.DB
	log    *logger
	token  string
	client *http.Client
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/api/companies", a.handleCompanies)
	mux.HandleFunc("/api/posts", a.handleListPosts)
	mux.HandleFunc("/api/posts/", a.handlePost)
	return a.middleware(mux)
}

func (a *api) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				a.log.log(fmt.Sprintf("panic serving %s: %v", r.URL.Path, rec))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		if a.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or wrong API token")
				return
			}
		}
		next.ServeHTTP(w, r)
		if d := time.Since(start); d > time.Second {
			a.log.log(fmt.Sprintf("slow request: %s %s took %s", r.Method, r.URL.Path, d.Round(time.Millisecond)))
		}
	})
}

func (a *api) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	var posts int
	var lastCrawl sql.NullString
	if err := a.db.QueryRow(`SELECT (SELECT COUNT(*) FROM posts), (SELECT MAX(last_success) FROM feeds)`).
		Scan(&posts, &lastCrawl); err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "posts": posts, "lastCrawl": lastCrawl.String})
}

func (a *api) handleCompanies(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	companies, err := listCompanies(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, companies)
}

func (a *api) handleListPosts(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	f := postFilter{
		company: strings.TrimSpace(q.Get("company")),
		query:   q.Get("q"),
		unread:  q.Get("unread") == "1" || q.Get("unread") == "true",
		limit:   clampInt(q.Get("limit"), defaultPageSize, 1, maxPageSize),
		offset:  clampInt(q.Get("offset"), 0, 0, 1<<30),
	}
	posts, total, err := listPosts(a.db, f)
	if err != nil {
		a.serverError(w, err)
		return
	}
	var next *int
	if end := f.offset + len(posts); end < total {
		next = &end
	}
	writeJSON(w, http.StatusOK, map[string]any{"posts": posts, "total": total, "nextOffset": next})
}

// /api/posts/{id} and /api/posts/{id}/read
func (a *api) handlePost(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/posts/"), "/")
	id, sub, _ := strings.Cut(rest, "/")
	switch {
	case id == "":
		writeError(w, http.StatusNotFound, "not found")
	case sub == "":
		a.handlePostDetail(w, r, id)
	case sub == "read":
		a.handleRead(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (a *api) handlePostDetail(w http.ResponseWriter, r *http.Request, id string) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	sp, err := getPost(a.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}
	if err != nil {
		a.serverError(w, err)
		return
	}

	full := sp.contentHTML.Valid
	contentHTML := sp.contentHTML.String
	if !full {
		// Not downloaded yet (or the crawler gave up): try now so the post opens with full text.
		var fetchErr error
		if sp.attempts < maxContentAttempts {
			ctx, cancel := context.WithTimeout(r.Context(), onDemandFetchMax)
			art, err := fetchArticle(ctx, a.client, sp.post.URL, sp.post.Title)
			cancel()
			if err == nil {
				if err := saveContent(a.db, id, art); err != nil {
					a.log.log(fmt.Sprintf("saving content for %s: %v", id, err))
				} else if fresh, err := getPost(a.db, id); err == nil {
					sp = fresh // picks up reading time, byline and summary
				}
				full, contentHTML = true, art.html
			} else if r.Context().Err() == nil {
				fetchErr = err
				_ = saveContentFailure(a.db, id, err)
			}
		}
		if !full {
			contentHTML = fallbackHTML(sp.post, fetchErr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"post":          sp.post,
		"contentHtml":   contentHTML,
		"isFullArticle": full,
	})
}

func (a *api) handleRead(w http.ResponseWriter, r *http.Request, id string) {
	if !allowMethods(w, r, http.MethodPut, http.MethodPost, http.MethodDelete) {
		return
	}
	ok, err := setRead(a.db, id, r.Method != http.MethodDelete)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func fallbackHTML(p apiPost, cause error) string {
	var s strings.Builder
	if p.Summary != "" {
		fmt.Fprintf(&s, "<p>%s</p>\n", html.EscapeString(p.Summary))
	}
	s.WriteString(`<p><em>The full article couldn't be downloaded`)
	if cause != nil {
		fmt.Fprintf(&s, " (%s)", html.EscapeString(cause.Error()))
	}
	fmt.Fprintf(&s, `. <a href="%s">Read it on the original blog.</a></em></p>`, html.EscapeString(p.URL))
	return s.String()
}

func (a *api) serverError(w http.ResponseWriter, err error) {
	a.log.log(fmt.Sprintf("api error: %v", err))
	writeError(w, http.StatusInternalServerError, "internal error")
}

func allowMethods(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func clampInt(raw string, def, lo, hi int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
