package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func runCrawl(args []string) int {
	var dryRun, noNotify, showHelp bool
	var contentLimit int
	fs := flag.NewFlagSet("eng-blog-bot", flag.ContinueOnError)
	fs.BoolVar(&dryRun, "dry-run", false, "fetch and print the digest/message, but don't save anything or send")
	fs.BoolVar(&noNotify, "no-notify", false, "save new posts but don't send a message")
	fs.IntVar(&contentLimit, "content-limit", defaultContentLimit, "max full articles to download this run")
	fs.BoolVar(&showHelp, "h", false, "show help")
	parseErr := fs.Parse(args)
	if showHelp || parseErr == flag.ErrHelp {
		printUsage()
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
			st, _ := getFeedState(db, cfg) // zero state on error = plain unconditional fetch
			states[i] = st
			outcomes[i] = fetchFeed(client, cfg, st.cache)
		}(i, cfg)
	}
	wg.Wait()

	var anySeeded bool
	_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM feeds WHERE seeded_at IS NOT NULL)`).Scan(&anySeeded)

	now := time.Now().Format(localTimeLayout)
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
	commit := func() bool {
		if err := tx.Commit(); err != nil {
			log.log(fmt.Sprintf("fatal: %v", err))
			return false
		}
		committed = true
		return true
	}

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
		if _, err := tx.Exec(`UPDATE feeds SET company = ? WHERE name = ?`, cfg.company, cfg.name); err != nil {
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

	if noNotify && !dryRun {
		for _, ns := range newlySeeded {
			log.log(fmt.Sprintf("now following %s (%s indexed)", ns.name, plural(ns.fresh, "existing post")))
		}
		log.log(fmt.Sprintf("no-notify: %s waiting for the next digest", plural(len(pendingList), "post")))
		if !commit() {
			return 1
		}
		downloadArticles(db, log, contentLimit)
		return exitCode(failures, len(feeds))
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
			if !commit() {
				return 1
			}
			downloadArticles(db, log, contentLimit)
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
	if !commit() {
		return 1
	}
	downloadArticles(db, log, contentLimit)
	return exitCode(failures, len(feeds))
}

// downloadArticles fetches full text for posts whose feed only had an excerpt.
// Runs after the crawl transaction is committed, so a slow blog never delays
// the digest. Hosts are fetched in parallel; requests to one host are spaced out.
func downloadArticles(db *sql.DB, log *logger, limit int) {
	if limit <= 0 {
		return
	}
	jobs, err := contentQueue(db, limit)
	if err != nil {
		log.log(fmt.Sprintf("articles: listing queue failed: %v", err))
		return
	}
	if len(jobs) == 0 {
		return
	}
	byHost := map[string][]contentJob{}
	var hosts []string
	for _, j := range jobs {
		h := j.link
		if u, err := url.Parse(j.link); err == nil {
			h = u.Host
		}
		if _, ok := byHost[h]; !ok {
			hosts = append(hosts, h)
		}
		byHost[h] = append(byHost[h], j)
	}

	ctx, cancel := context.WithTimeout(context.Background(), contentPhaseBudget)
	defer cancel()
	client := &http.Client{Timeout: httpTimeout}
	var mu sync.Mutex
	saved, failed := 0, 0
	hostCh := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < contentWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range hostCh {
				streak := 0 // consecutive failures on this host
				for i, j := range byHost[h] {
					if streak >= hostFailureStreak {
						break // host is probably blocking us; leave the rest for next run
					}
					if i > 0 {
						select {
						case <-ctx.Done():
						case <-time.After(contentHostDelay):
						}
					}
					if ctx.Err() != nil {
						break
					}
					art, err := fetchArticle(ctx, client, j.link, j.title)
					if ctx.Err() != nil {
						break // out of time; don't count it as a failed attempt
					}
					if err == nil {
						err = saveContent(db, j.id, art)
						if err != nil {
							log.log(fmt.Sprintf("articles: saving %s: %v", j.link, err))
						}
					} else if serr := saveContentFailure(db, j.id, err); serr != nil {
						log.log(fmt.Sprintf("articles: saving failure for %s: %v", j.link, serr))
					}
					mu.Lock()
					if err == nil {
						saved++
						streak = 0
					} else {
						failed++
						streak++
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, h := range hosts {
		hostCh <- h
	}
	close(hostCh)
	wg.Wait()

	msg := fmt.Sprintf("articles: %d downloaded, %d failed", saved, failed)
	if left := len(jobs) - saved - failed; left > 0 {
		msg += fmt.Sprintf(", %d left for next run", left)
	}
	log.log(msg)
}

// Non-zero only when every feed failed (likely no network), so the scheduler
// marks the run as failed without crying wolf over one flaky blog.
func exitCode(failures, total int) int {
	if total > 0 && failures == total {
		return 1
	}
	return 0
}
