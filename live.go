package main

import (
	"io"
	"sort"
	"sync"
	"time"
)

// liveSync is the in-memory progress of the running sync, shown in the web
// UI's progress panel. Nothing here is persisted.
type liveSync struct {
	mu       sync.Mutex
	running  bool
	start    time.Time
	end      time.Time
	total    int
	done     int
	app      string
	planned  int64 // bytes the current app still has to download (announced)
	got      int64 // bytes received for the current app
	allBytes int64 // bytes received in this run
	files    map[string]*liveFile
	log      []string
	samples  []speedSample
}

type liveFile struct {
	Name string `json:"name"`
	Got  int64  `json:"got"`
	Size int64  `json:"size"`
}

type speedSample struct {
	t time.Time
	n int64
}

var live = &liveSync{}

func (l *liveSync) begin(total int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l = liveSync{running: true, start: time.Now(), total: total, files: map[string]*liveFile{}}
}

func (l *liveSync) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running, l.end, l.done, l.app, l.files = false, time.Now(), l.total, "", map[string]*liveFile{}
}

func (l *liveSync) setApp(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.app, l.planned, l.got = name, 0, 0
}

func (l *liveSync) appDone() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done++
	l.planned, l.got = 0, 0
}

func (l *liveSync) plan(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.planned += n
}

func (l *liveSync) note(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return
	}
	l.log = append(l.log, time.Now().Format("15:04:05")+"  "+msg)
	if len(l.log) > 60 {
		l.log = l.log[len(l.log)-60:]
	}
}

// track wraps a download body so its bytes show up in the panel.
func (l *liveSync) track(name string, size int64, r io.Reader) (io.Reader, func()) {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return r, func() {}
	}
	f := &liveFile{Name: name, Size: size}
	key := name + "#" + time.Now().Format("150405.000000000")
	l.files[key] = f
	l.mu.Unlock()
	return &liveReader{r: r, l: l, f: f}, func() {
		l.mu.Lock()
		delete(l.files, key)
		l.mu.Unlock()
	}
}

type liveReader struct {
	r io.Reader
	l *liveSync
	f *liveFile
}

func (x *liveReader) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	if n > 0 {
		x.l.mu.Lock()
		x.f.Got += int64(n)
		x.l.got += int64(n)
		x.l.allBytes += int64(n)
		x.l.mu.Unlock()
	}
	return n, err
}

// snapshot is what /api/status returns as sync.live.
func (l *liveSync) snapshot() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() {
		return nil
	}
	now := time.Now()
	l.samples = append(l.samples, speedSample{now, l.allBytes})
	for len(l.samples) > 1 && now.Sub(l.samples[0].t) > 8*time.Second {
		l.samples = l.samples[1:]
	}
	var speed int64
	if s := l.samples[0]; now.Sub(s.t) > 500*time.Millisecond {
		speed = int64(float64(l.allBytes-s.n) / now.Sub(s.t).Seconds())
	}
	frac := 0.0
	if l.planned > 0 {
		frac = float64(l.got) / float64(l.planned)
		if frac > 0.97 {
			frac = 0.97
		}
	}
	pct := 100.0
	if l.running && l.total > 0 {
		pct = (float64(l.done) + frac) / float64(l.total) * 100
	}
	files := make([]liveFile, 0, len(l.files))
	for _, f := range l.files {
		files = append(files, *f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	elapsed := now.Sub(l.start)
	if !l.running {
		elapsed = l.end.Sub(l.start)
	}
	return map[string]any{
		"running": l.running, "total": l.total, "done": l.done, "app": l.app,
		"percent": int(pct), "bytes": l.allBytes, "speed": speed, "files": files,
		"log": append([]string(nil), l.log...), "elapsed": int(elapsed.Seconds()),
		"ended": l.end, "started": l.start.Format(time.RFC3339Nano),
	}
}
