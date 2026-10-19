# eng-blog-bot (Go)

Go port of the daily engineering-blog digest crawler. Same job, same files
(`~/Development/eng-blog-bot/{seen.db,digests/,crawl.log}`), same `musegadget
send-user-msg` notification. It reuses your existing `seen.db` as-is: the first
run migrates it in place and doesn't re-announce anything.

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
- **Optional `feeds.txt`**: change feeds without recompiling (`Name | URL`,
  one per line; keep names identical so history still matches).
- Exits non-zero only when *every* feed failed (likely no network).

## Build & run on the Pi

```bash
cd ~/Development/eng-blog-bot-go
go build -o eng-blog-bot .
./eng-blog-bot --dry-run   # fetches and prints the digest and message; saves and sends nothing
cp eng-blog-bot ~/.local/bin/
```

Environment overrides: `ENG_BLOG_BOT_DIR` (data dir) and `MUSEGADGET_BIN`
(full path to `musegadget` if it isn't on `PATH`).

Requires Go 1.23+ and a C compiler (for go-sqlite3 via cgo).
