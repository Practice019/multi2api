package logbuf

import (
	"sync"
	"testing"
	"time"
)

type reliabilityWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *reliabilityWriter) WriteLog(Entry) error {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return nil
}
func reliabilityWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked")
	}
}

func TestReliabilitySQLCloseWaits(t *testing.T) {
	w := &reliabilityWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(w.release) })
	s := newSQLSink(w, 8)
	s.enqueue(Entry{})
	reliabilityWait(t, w.entered)
	s.enqueue(Entry{})
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); s.close() }()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("close returned before worker")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(w.release) })
	reliabilityWait(t, done)
	s.enqueue(Entry{})
	s.flush()
	stats := s.stats()
	if stats.Written != 2 || stats.Dropped != 1 || stats.Queued != 0 {
		t.Fatalf("%+v", stats)
	}
}

func TestReliabilitySQLStopDoesNotHoldRing(t *testing.T) {
	r := New(8)
	w := &reliabilityWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(w.release) })
	r.SetSQLWriter(w)
	r.Push(Entry{})
	reliabilityWait(t, w.entered)
	done := make(chan struct{})
	go func() { r.StopSQL(); close(done) }()
	pushed := make(chan struct{})
	go func() { r.Push(Entry{}); r.SQLWriter(); close(pushed) }()
	reliabilityWait(t, pushed)
	select {
	case <-done:
		t.Fatal("stop returned early")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(w.release) })
	reliabilityWait(t, done)
	r.StopSQL()
}
