# eng-blog-bot (Go)

Go port of the daily engineering-blog digest crawler, and the backend for the
Eng Blogs Android app. Same job, same files
(`~/Development/eng-blog-bot/{seen.db,digests/,crawl.log}`), same `musegadget
send-user-msg` notification. It reuses your existing `seen.db` as-is: the first
run migrates it in place and doesn't re-announce anything.

## App backend

- **38 blogs** by default (see `feeds.txt`): Stripe, Spotify, Pinterest,
  Shopify, Lyft, Slack, GitHub, Dropbox, Zalando, Datadog, Jane Street, Android
  Developers, Kotlin and more. DoorDash blocks crawlers and Uber has no feed, so
  neither can be added.
- **Full articles stored on the Pi.** Posts whose feed carries the whole article
  (Medium, many WordPress blogs) are saved immediately. For the others, each run
  downloads up to 150 pages (`--content-limit`) and extracts the article with
  Readability. Downloads run in parallel across sites, with one request per
  second to any single site, and a 10-minute cap. Opening a post in the app that
  isn't downloaded yet fetches it on the spot.
- **Per-post summary, author, tags and reading time**, and read/unread state
  synced from the app.
- **`eng-blog-bot serve`**: the HTTP API the app talks to (see below).
- **`--no-notify`**: crawl without messaging. New posts reach the app right
  away and still go out in the next regular digest.
- **SQLite in WAL mode**, so the API can read while a crawl writes.

## Improvements over the Python version

- **Parallel fetching** with a 30 s timeout per feed (the Python version fetched
  one feed at a time with no timeout — a stalled server hung the whole job).
- **Conditional GET** (ETag / Last-Modified): unchanged feeds return 304 with no
  body, saving bandwidth and parse time.
- **Failed notifications don't lose posts**: if `musegadget` fails, posts stay
  pending and go out on the next run; the digest file is rolled back so nothing
  is duplicated.
- **Dedupe by link as well as guid**: feeds that change their guid format no
  longer flood the digest.
- **Failure alerts**: one ⚠ alert after a feed fails 3 runs in a row, reset
  when it recovers.
- **`MAX_PER_FEED_IN_DIGEST`** (defined but unused in Python) now caps each
  feed at 8 entries ("…and N more").
- **New feeds** are indexed silently with a "Now following: …" note instead of
  dumping their back catalogue into the digest.
- **Log rotation**: `crawl.log` rotates to `crawl.log.1` at 1 MiB.
- **Whole run in one SQLite transaction** (fewer SD-card writes).
- **Optional `feeds.txt`**: change feeds without recompiling
  (`Name | URL | Company`, one per line; the company column is optional; keep
  names identical so history still matches).
- Exits non-zero only when *every* feed failed (likely no network).

## Build & run on the Pi

```bash
cd ~/Development/eng-blog-bot-go
go mod tidy                # first time after pulling: fills go.sum for the new dependencies
go build -o eng-blog-bot .
go test ./...
./eng-blog-bot --dry-run   # fetches and prints the digest and message; saves and sends nothing
cp eng-blog-bot ~/.local/bin/
```

Commands:

```
eng-blog-bot [--dry-run] [--no-notify] [--content-limit N]   crawl (default)
eng-blog-bot serve [--addr :8080]                              HTTP API for the app
```

Environment overrides: `ENG_BLOG_BOT_DIR` (data dir), `MUSEGADGET_BIN`
(full path to `musegadget` if it isn't on `PATH`), and `ENG_BLOG_API_TOKEN`
(if set, the API requires `Authorization: Bearer <token>`).

Requires Go 1.25+ and a C compiler (for go-sqlite3 via cgo).

## Setting up the app backend

1. **Feeds**: copy `feeds.txt` into the data dir. If you already have one there,
   add a `| Company` column to your lines and keep the names exactly as they are.
2. **Morning digest**: keep your existing scheduled `eng-blog-bot` run. The first
   run after upgrading sends one "Now following: …" message for the new blogs.
   Their back catalogs (about 1,300 posts) are indexed without being announced.
3. **API and hourly crawl**: install the systemd user units in `deploy/`:

   ```bash
   mkdir -p ~/.config/systemd/user
   cp deploy/*.service deploy/*.timer ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now eng-blog-api eng-blog-crawl.timer
   sudo loginctl enable-linger "$USER"   # keep them running without a login session
   curl http://localhost:8080/api/health
   ```

   The hourly crawl runs at half past the hour, so it doesn't collide with a
   digest job scheduled on the hour.
4. **App**: in the Android project's `local.properties`, set
   `engblogs.serverUrl=http://<pi-ip>:8080/` and rebuild.

The first run downloads full text for about 150 posts. The hourly runs finish
the back catalog within a day. Storage is about 15 MB per 1,000 posts.

## API

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/api/health` | `{ok, posts, lastCrawl}` |
| GET | `/api/posts?limit=30&offset=0&company=&q=&unread=1` | newest first; `{posts, total, nextOffset}` |
| GET | `/api/posts/{id}` | `{post, contentHtml, isFullArticle}` |
| PUT / DELETE | `/api/posts/{id}/read` | mark read / unread |
| GET | `/api/companies` | `[{name, postCount}]`, most posts first |

There's no login by default, which is fine on a home network. If you expose the
API beyond your LAN (e.g. over Tailscale), set `ENG_BLOG_API_TOKEN` in
`deploy/eng-blog-api.service`. Then set the same value as `engblogs.apiToken` in
the app's `local.properties`.
