package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// read answers view_only only for a boss role viewing another mailbox; a
// slot-1 session reading its own mail (stored under the base name), and a
// slot-2 session, are never marked (#365).
func TestReadViewOnlyFollowsTheServerDecision(t *testing.T) {
	s, clock := slotsWithRoles(t)
	read := func(as, header string, id int64) (wire.Message, int) {
		w := callAs(s, "GET", fmt.Sprintf("/api/messages/%d", id), tokenFor(as), header, nil)
		var m wire.Message
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return m, w.Code
	}
	status := func(id int64) string {
		m, err := s.mail().View(id)
		if err != nil {
			t.Fatal(err)
		}
		return m.Status
	}

	toBase := apiMail(t, s, clock, "team/worker", "/api/messages", map[string]any{"recipient": "hq/boss", "subject": "s", "body": "b"})
	if m, code := read("hq/boss", "hq/boss-1", toBase.ID); code != 200 || m.ViewOnly || status(toBase.ID) != "read" {
		t.Fatalf("slot 1 own mail: %d view_only=%v status=%s", code, m.ViewOnly, status(toBase.ID))
	}
	toSlot2 := apiMail(t, s, clock, "team/worker", "/api/messages", map[string]any{"recipient": "hq/boss-2", "subject": "s", "body": "b"})
	if m, code := read("hq/boss", "hq/boss-2", toSlot2.ID); code != 200 || m.ViewOnly || status(toSlot2.ID) != "read" {
		t.Fatalf("slot 2 own mail: %d view_only=%v status=%s", code, m.ViewOnly, status(toSlot2.ID))
	}

	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('hq/boss2','secret-hq/boss2','now')`); err != nil {
		t.Fatal(err)
	}
	s.bossRoles["hq/boss2"] = true
	other := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "s", "body": "b"})
	if m, code := read("hq/boss2", "hq/boss2", other.ID); code != 200 || !m.ViewOnly || status(other.ID) != "pending" {
		t.Fatalf("boss views other: %d view_only=%v status=%s", code, m.ViewOnly, status(other.ID))
	}
	// The owner's own read is not marked.
	if m, code := read("team/worker", "team/worker", other.ID); code != 200 || m.ViewOnly {
		t.Fatalf("recipient read: %d view_only=%v", code, m.ViewOnly)
	}

	// inbox --of: marked for another mailbox, plain for the caller's own.
	list := func(as, header, of string) []wire.Message {
		w := callAs(s, "GET", "/api/messages?box=inbox&of="+of, tokenFor(as), header, nil)
		var ms []wire.Message
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ms) != nil {
			t.Fatalf("list of %s: %d %s", of, w.Code, w.Body)
		}
		return ms
	}
	if ms := list("hq/boss2", "hq/boss2", "team/worker"); len(ms) == 0 || !ms[0].ViewOnly {
		t.Fatalf("list of another mailbox: %+v", ms)
	}
	if ms := list("hq/boss", "hq/boss-1", "hq/boss"); len(ms) == 0 || ms[0].ViewOnly {
		t.Fatalf("list of own mailbox: %+v", ms)
	}
}
