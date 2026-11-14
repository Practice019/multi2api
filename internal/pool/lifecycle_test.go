package pool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func awaitPersistence(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("operation blocked")
	}
}

func TestReliabilityFinalClose(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "stub"})
	p.SetCredits("stub", 42)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	select {
	case <-p.flusherDone:
	default:
		t.Fatal("flusher alive")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	if err = json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	if sf.Accounts["stub"].Credits != 42 {
		t.Fatal(string(raw))
	}
}

func TestReliabilityBlockedWriteOrderedSnapshot(t *testing.T) {
	p := New("")
	p.stateFp = filepath.Join(t.TempDir(), "state.json")
	defer p.Close()
	p.Add(&auth.Auth{UID: "stub"})
	p.SetQuota("stub", FromPerModel(map[string]int64{"m": 1}))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	calls := 0
	p.writeState = func(fp string, raw []byte) error {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return writeStateFile(fp, raw)
	}
	first := make(chan struct{})
	go func() { p.Flush(); close(first) }()
	awaitPersistence(t, entered)
	changed := make(chan struct{})
	go func() {
		p.mu.Lock()
		p.byUID["stub"].quota.ByModel["m"] = 2
		p.dirty.Store(true)
		p.mu.Unlock()
		p.List()
		close(changed)
	}()
	awaitPersistence(t, changed)
	second := make(chan struct{})
	go func() { p.Flush(); close(second) }()
	once.Do(func() { close(release) })
	awaitPersistence(t, first)
	awaitPersistence(t, second)
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	if err = json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	if sf.Accounts["stub"].Quota.ByModel["m"] != 2 || p.dirty.Load() {
		t.Fatal("new state lost")
	}
}

func TestReliabilityDirtyRetry(t *testing.T) {
	p := New("")
	p.stateFp = filepath.Join(t.TempDir(), "state.json")
	defer p.Close()
	p.Add(&auth.Auth{UID: "stub"})
	p.SetCredits("stub", 7)
	failure := errors.New("stub disk full")
	p.writeState = func(string, []byte) error { return failure }
	if !errors.Is(p.FlushErr(), failure) || !p.dirty.Load() {
		t.Fatal("failed write cleared dirty")
	}
	p.writeState = nil
	if err := p.FlushErr(); err != nil {
		t.Fatal(err)
	}
	if p.dirty.Load() {
		t.Fatal("successful retry left dirty")
	}
	if err := os.Mkdir(p.stateFp+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	p.SetCredits("stub", 8)
	if p.FlushErr() == nil || !p.dirty.Load() {
		t.Fatal("file failure cleared dirty")
	}
	if err := os.Remove(p.stateFp + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

type reliabilityStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *reliabilityStore) LoadState() ([]byte, bool) { return nil, false }
func (s *reliabilityStore) SaveState([]byte) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
}

func TestReliabilitySlowMirrorClose(t *testing.T) {
	p := New("")
	p.stateFp = filepath.Join(t.TempDir(), "state.json")
	defer p.Close()
	s := &reliabilityStore{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(s.release) })
	p.SetStore(s)
	p.Add(&auth.Auth{UID: "stub"})
	p.SetCredits("stub", 1)
	flushed := make(chan struct{})
	go func() { p.Flush(); close(flushed) }()
	awaitPersistence(t, s.entered)
	changed := make(chan struct{})
	go func() { p.SetCredits("stub", 2); p.List(); close(changed) }()
	awaitPersistence(t, changed)
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close did not wait for mirror")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(s.release) })
	awaitPersistence(t, flushed)
	awaitPersistence(t, closed)
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	if sf.Accounts["stub"].Credits != 2 {
		t.Fatal("final flush lost update")
	}
}

func TestReliabilityEncodingAndRenameFailure(t *testing.T) {
	p := New("")
	p.stateFp = filepath.Join(t.TempDir(), "state.json")
	defer p.Close()
	p.Add(&auth.Auth{UID: "stub"})
	p.SetCredits("stub", 1)
	if err := p.FlushErr(); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.byUID["stub"].until = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	p.dirty.Store(true)
	p.mu.Unlock()
	if p.FlushErr() == nil || !p.dirty.Load() {
		t.Fatal("encoding failure lost dirty")
	}
	raw, err := os.ReadFile(p.stateFp)
	if err != nil || !json.Valid(raw) {
		t.Fatal("previous file damaged")
	}
	p.ClearCooldown("stub")
	blocked := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	original := p.stateFp
	p.stateFp = blocked
	if p.FlushErr() == nil || !p.dirty.Load() {
		t.Fatal("rename failure lost dirty")
	}
	p.stateFp = original
	if err := p.FlushErr(); err != nil {
		t.Fatal(err)
	}
	if p.dirty.Load() {
		t.Fatal("retry did not clean state")
	}
}

func TestReliabilitySnapshotDeepCopy(t *testing.T) {
	for _, m := range []map[string]int64{{}, {"m": 1}} {
		p := New("")
		p.Add(&auth.Auth{UID: "stub"})
		p.SetQuota("stub", FromPerModel(m))
		p.mu.Lock()
		sf := p.stateOverviewLocked()
		p.byUID["stub"].quota.ByModel["m"] = 9
		p.mu.Unlock()
		if sf.Accounts["stub"].Quota.ByModel["m"] == 9 {
			t.Fatal("snapshot aliases quota")
		}
		p.Close()
	}
}
