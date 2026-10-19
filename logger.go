package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// logger writes to stdout and appends to crawl.log. Safe for concurrent use.
type logger struct {
	path string
	mu   sync.Mutex
}

func newLogger(path string) *logger {
	if st, err := os.Stat(path); err == nil && st.Size() > logMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	return &logger{path: path}
}

func (l *logger) log(msg string) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02 15:04:05"), msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Println(line)
	if l.path == "" {
		return
	}
	if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, line)
		_ = f.Close()
	}
}
