package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Used when <base>/feeds.txt doesn't exist. Keep in sync with feeds.txt.
// The first six names predate the company column: don't rename them, the
// name is the key that ties a feed to its already-seen posts in seen.db.
var defaultFeeds = []feedCfg{
	{"Cloudflare Blog", "https://blog.cloudflare.com/rss/", "Cloudflare"},
	{"Netflix TechBlog", "https://medium.com/feed/netflix-techblog", "Netflix"},
	{"Airbnb Engineering", "https://medium.com/feed/airbnb-engineering", "Airbnb"},
	{"Google Developers Blog", "https://developers.googleblog.com/rss/", "Google"},
	{"AWS Architecture Blog", "https://aws.amazon.com/blogs/architecture/feed/", "AWS"},
	{"Meta Engineering", "https://engineering.fb.com/feed/", "Meta"},
	{"Stripe Blog", "https://stripe.com/blog/feed.rss", "Stripe"},
	{"Spotify Engineering", "https://engineering.atspotify.com/feed/", "Spotify"},
	{"Pinterest Engineering", "https://medium.com/feed/pinterest-engineering", "Pinterest"},
	{"Dropbox Tech", "https://dropbox.tech/feed", "Dropbox"},
	{"Slack Engineering", "https://slack.engineering/feed/", "Slack"},
	{"GitHub Engineering", "https://github.blog/engineering/feed/", "GitHub"},
	{"Shopify Engineering", "https://shopify.engineering/blog.atom", "Shopify"},
	{"Lyft Engineering", "https://eng.lyft.com/feed", "Lyft"},
	{"Instacart Tech", "https://tech.instacart.com/feed", "Instacart"},
	{"Grab Engineering", "https://engineering.grab.com/feed.xml", "Grab"},
	{"Etsy Code as Craft", "https://www.etsy.com/codeascraft/rss", "Etsy"},
	{"Expedia Group Tech", "https://medium.com/feed/expedia-group-tech", "Expedia"},
	{"Booking.com Engineering", "https://medium.com/feed/booking-com-development", "Booking.com"},
	{"Zalando Engineering", "https://engineering.zalando.com/atom.xml", "Zalando"},
	{"Yelp Engineering", "https://engineeringblog.yelp.com/feed.xml", "Yelp"},
	{"Stack Overflow Engineering", "https://stackoverflow.blog/engineering/feed/", "Stack Overflow"},
	{"Salesforce Engineering", "https://engineering.salesforce.com/feed/", "Salesforce"},
	{"Atlassian Engineering", "https://www.atlassian.com/engineering/feed", "Atlassian"},
	{"Datadog Engineering", "https://www.datadoghq.com/blog/engineering/index.xml", "Datadog"},
	{"Canva Engineering", "https://www.canva.dev/blog/engineering/feed.xml", "Canva"},
	{"Block Engineering", "https://engineering.block.xyz/blog/rss.xml", "Block"},
	{"Nubank Engineering", "https://building.nubank.com/feed/", "Nubank"},
	{"Mercari Engineering", "https://engineering.mercari.com/en/blog/feed.xml", "Mercari"},
	{"Wix Engineering", "https://www.wix.engineering/blog-feed.xml", "Wix"},
	{"Jane Street Tech Blog", "https://blog.janestreet.com/feed.xml", "Jane Street"},
	{"Mozilla Hacks", "https://hacks.mozilla.org/feed/", "Mozilla"},
	{"Fly.io Blog", "https://fly.io/blog/feed.xml", "Fly.io"},
	{"Tailscale Blog", "https://tailscale.com/blog/index.xml", "Tailscale"},
	{"PlanetScale Blog", "https://planetscale.com/blog/rss.xml", "PlanetScale"},
	{"Sentry Blog", "https://blog.sentry.io/feed.xml", "Sentry"},
	{"HashiCorp Blog", "https://www.hashicorp.com/blog/feed.xml", "HashiCorp"},
	{"Android Developers Blog", "https://android-developers.googleblog.com/atom.xml", "Android"},
	{"Kotlin Blog", "https://blog.jetbrains.com/kotlin/feed/", "Kotlin"},
}

const userAgent = "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36 eng-blog-bot/1.0"

const (
	maxPerFeedInDigest = 8
	highlightsPerFeed  = 2
	maxHighlights      = 8
	httpTimeout        = 30 * time.Second
	maxFeedBytes       = 10 * 1024 * 1024
	museTimeout        = 30 * time.Second
	failureAlertAfter  = 3 // alert once a feed has failed this many runs in a row
	logMaxBytes        = 1024 * 1024

	defaultContentLimit = 150              // articles downloaded per crawl run
	contentWorkers      = 4                // hosts fetched in parallel
	contentHostDelay    = 1 * time.Second  // pause between requests to the same host
	contentPhaseBudget  = 10 * time.Minute // stop downloading articles after this
	maxContentAttempts  = 3                // give up on an article after this many failures
	hostFailureStreak   = 3                // skip the rest of a host this run after this many failures in a row
	maxArticleBytes     = 5 * 1024 * 1024
)

type feedCfg struct {
	name    string // key in seen.db; never rename an existing feed
	url     string
	company string // shown in the app; defaults to name
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

// feeds.txt format: one "Name | URL" or "Name | URL | Company" per line,
// "#" starts a comment. Lets you add or remove blogs without recompiling.
func loadFeeds(p paths, log *logger) ([]feedCfg, error) {
	data, err := os.ReadFile(p.feedsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultFeeds, nil
		}
		return nil, fmt.Errorf("reading feeds.txt: %w", err)
	}
	feeds, problems := parseFeeds(string(data))
	for _, msg := range problems {
		log.log("feeds.txt " + msg)
	}
	if len(feeds) == 0 {
		return nil, fmt.Errorf("%s has no usable feeds", p.feedsFile)
	}
	return feeds, nil
}

func parseFeeds(data string) (feeds []feedCfg, problems []string) {
	seen := map[string]bool{}
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			problems = append(problems, fmt.Sprintf("line %d: expected `Name | URL [| Company]`, skipped", i+1))
			continue
		}
		if seen[parts[0]] {
			problems = append(problems, fmt.Sprintf("line %d: duplicate feed name %q, skipped", i+1, parts[0]))
			continue
		}
		seen[parts[0]] = true
		cfg := feedCfg{name: parts[0], url: parts[1], company: parts[0]}
		if len(parts) == 3 && parts[2] != "" {
			cfg.company = parts[2]
		}
		feeds = append(feeds, cfg)
	}
	return feeds, problems
}
