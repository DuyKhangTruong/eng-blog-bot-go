package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "[", "\\[")
	return strings.ReplaceAll(s, "]", "\\]")
}

func shortDate(published sql.NullString) string {
	if t := parsePublished(published.String); t != nil {
		return t.Format("2006-01-02")
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
