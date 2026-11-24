package main

import (
	"strings"
	"testing"
)

func TestLiveSyncCycle(t *testing.T) {
	l := &liveSync{}
	for i := 0; i < 2; i++ {
		l.begin(2)
		l.setApp("a")
		l.plan(10)
		r, done := l.track("f", 10, strings.NewReader("0123456789"))
		buf := make([]byte, 4)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
		done()
		l.note("x")
		if s := l.snapshot(); s["bytes"].(int64) != 10 {
			t.Fatalf("bytes %v", s["bytes"])
		}
		l.appDone()
		l.finish()
		if s := l.snapshot(); s["percent"].(int) != 100 {
			t.Fatalf("percent %v", s["percent"])
		}
	}
}
