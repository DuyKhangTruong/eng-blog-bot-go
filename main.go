// Command eng-blog-bot crawls engineering blogs and serves them to the
// Eng Blogs Android app.
//
// It fetches RSS/Atom feeds of engineering blogs in parallel, tracks posts in
// SQLite, stores each post's summary, tags and full article text, writes a
// daily markdown digest, and notifies the user via the `musegadget` CLI.
//
// Drop-in compatible with the Python version's seen.db: the first run
// migrates the schema in place and does not re-announce old posts.
//
// Usage:
//
//	eng-blog-bot [--dry-run] [--no-notify] [--content-limit N]   crawl (default)
//	eng-blog-bot serve [--addr :8080]                              HTTP API for the app
//
// Typical setup on the Pi: the morning timer runs `eng-blog-bot` (crawl +
// digest + message), an hourly timer runs `eng-blog-bot --no-notify` (crawl
// only; new posts wait for the next digest), and `eng-blog-bot serve` runs
// as a service. All three share seen.db safely (WAL mode).
//
// Environment:
//
//	ENG_BLOG_BOT_DIR    data directory (default ~/Development/eng-blog-bot)
//	MUSEGADGET_BIN      full path to musegadget (default "musegadget" on PATH)
//	ENG_BLOG_API_TOKEN  if set, the API requires "Authorization: Bearer <token>"
package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		os.Exit(runServe(args[1:]))
	}
	if len(args) > 0 && args[0] == "crawl" {
		args = args[1:]
	}
	os.Exit(runCrawl(args))
}

const usage = `eng-blog-bot [crawl] [--dry-run] [--no-notify] [--content-limit N]
eng-blog-bot serve [--addr :8080]

crawl (default):
  --dry-run          fetch and print the digest/message, but don't save anything or send
  --no-notify        save new posts but don't send a message; they go into the next digest
  --content-limit N  max full articles to download this run (default 150, 0 = none)

serve:
  --addr ADDR        listen address (default :8080)

Env: ENG_BLOG_BOT_DIR (default ~/Development/eng-blog-bot), MUSEGADGET_BIN (default musegadget),
     ENG_BLOG_API_TOKEN (optional bearer token for the API)`

func printUsage() { fmt.Println(usage) }
