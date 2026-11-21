package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

const (
	recA = "hs_0000000000000000000000000000000a"
	recB = "hs_0000000000000000000000000000000b"
)

func TestProfilesWriteHeaderAndPrune(t *testing.T) {
	p := &Profiles{Dir: filepath.Join(t.TempDir(), "switchboard"), Endpoint: "http://100.64.0.10:8090"} // leak-guard:allow
	if got := p.Header(); got != "-" {
		t.Errorf("empty header = %q", got)
	}
	if err := p.Write("../x", wire.NodeCredential{ID: "rc_1", Role: "team/worker", Token: "t"}); err == nil {
		t.Error("a bad record id was written")
	}
	for _, c := range []struct {
		rec string
		cr  wire.NodeCredential
	}{
		{recA, wire.NodeCredential{ID: "rc_1", Role: "team/worker", Token: "t1"}},
		{recA, wire.NodeCredential{ID: "rc_2", Role: "team/worker", Token: "t2"}}, // replaces
		{recB, wire.NodeCredential{ID: "rc_3", Role: "team/boss", Token: "t3"}},
	} {
		if err := p.Write(c.rec, c.cr); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.Header(); got != "rc_2,rc_3" {
		t.Errorf("header = %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(p.Dir, "hosted-"+recA+".json"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v %v", fi, err)
		}
	}
	// apply points held records at their profile and drops the rest.
	other := "hs_0000000000000000000000000000000c"
	recs := []Record{{ID: recA, Profile: "team-a"}, {ID: other, Profile: "mine"}}
	p.apply(recs)
	if recs[0].Profile != "hosted-"+recA || recs[1].Profile != "mine" {
		t.Errorf("profiles %q %q", recs[0].Profile, recs[1].Profile)
	}
	if p.Has(recB) {
		t.Error("the profile of an unassigned record stayed")
	}
	if got := p.Header(); got != "rc_2" {
		t.Errorf("header after prune = %q", got)
	}
}

// A profile whose removal fails (on Windows: another process has it open) is
// removed once the holder lets go, without a new assignment.
func TestProfilesRemovalRetriesWhileFileIsOpen(t *testing.T) {
	p := &Profiles{Dir: filepath.Join(t.TempDir(), "switchboard"), Endpoint: "http://100.64.0.10:8090", retry: 10 * time.Millisecond} // leak-guard:allow
	if err := p.Write(recA, wire.NodeCredential{ID: "rc_1", Role: "team/worker", Token: "t1"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p.path(recA))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p.apply(nil)
	time.Sleep(50 * time.Millisecond)
	f.Close()
	waitFor(t, "the profile removed", func() bool { _, err := os.Stat(p.path(recA)); return os.IsNotExist(err) })
}

func TestProfilesRemovalRetryNeverDeletesANewCredential(t *testing.T) {
	var mu sync.Mutex
	block := true
	var logged []string
	p := &Profiles{Dir: filepath.Join(t.TempDir(), "switchboard"), Endpoint: "http://100.64.0.10:8090", retry: 5 * time.Millisecond, // leak-guard:allow
		Logf: func(f string, a ...any) { mu.Lock(); logged = append(logged, fmt.Sprintf(f, a...)); mu.Unlock() }}
	p.remove = func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		if block {
			return errors.New("sharing violation")
		}
		return os.Remove(path)
	}
	if err := p.Write(recA, wire.NodeCredential{ID: "rc_1", Role: "team/worker", Token: "t1"}); err != nil {
		t.Fatal(err)
	}
	p.apply(nil)
	mu.Lock()
	n := len(logged)
	mu.Unlock()
	if n != 1 {
		t.Errorf("logged %d lines, want 1", n)
	}
	// The record is assigned again and gets a new credential.
	if err := p.Write(recA, wire.NodeCredential{ID: "rc_2", Role: "team/worker", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	block = false
	mu.Unlock()
	waitFor(t, "the retry goroutine to end", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return !p.running })
	if got := p.Header(); got != "rc_2" {
		t.Errorf("header = %q, the new credential was removed", got)
	}
}
