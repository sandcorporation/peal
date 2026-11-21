package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// bossSend sends from hq/boss to to without moving the clock.
func bossSend(t *testing.T, s *Server, to, subject string) wire.Message {
	t.Helper()
	w := callAs(s, "POST", "/api/messages", tokenFor("hq/boss"), "hq/boss", map[string]any{"recipient": to, "subject": subject})
	var m wire.Message
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &m) != nil {
		t.Fatalf("send to %s: %d %s", to, w.Code, w.Body)
	}
	return m
}

// heldNotices lists the subjects of the [미수신] held-mail notices to box.
func heldNotices(t *testing.T, s *Server, box, kind string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT subject FROM messages WHERE sender=? AND recipient=? AND idem LIKE ? ORDER BY id`, identity.SystemSender, box, kind+":%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var subject string
		if err = rows.Scan(&subject); err != nil {
			t.Fatal(err)
		}
		out = append(out, subject)
	}
	return out
}

// A session's unread mail is held for it when its lease ends: the next
// session that gets the number (another folder) does not see it, the sender
// is told once, and the session gets it when it comes back (#354).
func TestHandoffHoldsEndedSlotMail(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, role, box string
		// keep registers a live slot 1 first, so the ended session holds number 2.
		keep bool
	}{
		{"slot 1", "team/worker-1", "team/worker", false},
		{"slot 2", "team/worker-2", "team/worker-2", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, clock := slotsWithRoles(t)
			if c.keep {
				slotDo(t, s, "register", ident("m", "/keep"))
			}
			if g := slotDo(t, s, "register", ident("m", "/x")); g.Role != c.role {
				t.Fatalf("A got %s, want %s", g.Role, c.role)
			}
			d := bossSend(t, s, c.box, "do X")
			clock.add(2 * time.Minute) // A's lease (1m) ends
			if c.keep {
				slotDo(t, s, "heartbeat", ident("m", "/keep"))
			}
			if g := slotDo(t, s, "register", ident("m", "/y")); g.Role != c.role {
				t.Fatalf("B got %s, want %s", g.Role, c.role)
			}
			if got := inboxSubjects(t, s, c.box); len(got) != 0 {
				t.Fatalf("B's inbox has A's mail: %v", got)
			}
			want := []string{"[미수신] " + c.box + " 받는 세션 없음 — #" + itoa(d.ID) + " do X"}
			if got := heldNotices(t, s, "hq/boss", "handoff-held"); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("sender notices %q, want %q", got, want)
			}
			// A comes back (its number went to B) and gets the mail.
			g := slotDo(t, s, "register", ident("m", "/x"))
			if got := inboxSubjects(t, s, g.Role); strings.Join(got, ",") != "do X" {
				t.Fatalf("A (%s) inbox: %v", g.Role, got)
			}
			if got := inboxSubjects(t, s, c.box); len(got) != 0 {
				t.Fatalf("B's inbox: %v", got)
			}
			if got := heldNotices(t, s, "hq/boss", "handoff-held"); len(got) != 1 {
				t.Fatalf("sender told again: %q", got)
			}
		})
	}
}

// Mail sent to a mailbox whose session ended is held at once; the send
// response names the mailbox state: live, ended and held, or (base with an
// empty slot 1) the live numbered slots (#354).
func TestSendReportsRecipientState(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)

	never := bossSend(t, s, "team/worker", "first")
	if st := never.RecipientState; st == nil || st.Live || st.Held || st.Mailbox != "team/worker" || never.Status != "pending" {
		t.Fatalf("never attached: %+v %+v", never, st)
	}

	slotDo(t, s, "register", ident("m", "/x")) // team/worker, takes "first"
	live := bossSend(t, s, "team/worker", "live")
	if st := live.RecipientState; st == nil || !st.Live || st.Held || len(st.LiveSlots) != 0 {
		t.Fatalf("live: %+v", st)
	}

	slotDo(t, s, "register", ident("m", "/y")) // team/worker-2
	clock.add(50 * time.Second)
	slotDo(t, s, "heartbeat", ident("m", "/y"))
	clock.add(5*time.Minute - 50*time.Second)   // /x's lease ended 4m ago
	slotDo(t, s, "heartbeat", ident("m", "/y")) // holds /x's unread "first" and "live"
	ended := bossSend(t, s, "team/worker", "ended")
	st := ended.RecipientState
	if st == nil || st.Live || !st.Held || st.EndedS != 4*60 || ended.Status != "held" || ended.AddressedTo != "team/worker" {
		t.Fatalf("ended: %+v %+v", ended, st)
	}
	if strings.Join(st.LiveSlots, ",") != "team/worker-2" {
		t.Fatalf("live slots: %v", st.LiveSlots)
	}
	if got := heldNotices(t, s, "hq/boss", "handoff-held"); len(got) != 3 || !strings.Contains(got[2], "#"+itoa(ended.ID)+" ended") {
		t.Fatalf("sender notices: %q", got)
	}
	if got := inboxSubjects(t, s, "team/worker"); len(got) != 0 {
		t.Fatalf("team/worker inbox: %v", got)
	}
	g := slotDo(t, s, "register", ident("m", "/x"))
	if got := inboxSubjects(t, s, g.Role); strings.Join(got, ",") != "ended,live,first" { // newest first
		t.Fatalf("A (%s) inbox: %v", g.Role, got)
	}
}

// Held mail that no session takes within SWITCHBOARD_HANDOFF_EXPIRE is
// ended: complete where it is, kept, and its sender gets one line (#354).
func TestHandoffExpires(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)
	s.handoffTTL = time.Hour
	slotDo(t, s, "register", ident("m", "/x"))
	clock.add(2 * time.Minute)
	m := bossSend(t, s, "team/worker", "late")
	if m.Status != "held" {
		t.Fatalf("not held: %+v", m)
	}
	clock.add(30 * time.Minute)
	if n := sweepOnce(t, s).Handoffs; n != 0 {
		t.Fatalf("before the limit: ended %d", n)
	}
	clock.add(31 * time.Minute)
	if n := sweepOnce(t, s).Handoffs; n != 1 {
		t.Fatalf("past the limit: ended %d, want 1", n)
	}
	if n := sweepOnce(t, s).Handoffs; n != 0 {
		t.Fatalf("second sweep ended %d", n)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM messages WHERE id=?`, m.ID).Scan(&status); err != nil || status != "complete" {
		t.Fatalf("expired mail: %q %v", status, err)
	}
	if got := heldNotices(t, s, "hq/boss", "handoff-expired"); len(got) != 1 || !strings.HasPrefix(got[0], "[미수신] #"+itoa(m.ID)+" 끝냄") {
		t.Fatalf("expiry notices: %q", got)
	}
	g := slotDo(t, s, "register", ident("m", "/x"))
	if got := inboxSubjects(t, s, g.Role); len(got) != 0 {
		t.Fatalf("expired mail delivered: %v", got)
	}
}

// The [미수신] sweep looks at the receiving mailbox: mail to a slot with no
// session is noticed even while another slot of the base is live (#354).
func TestNoSessionNoticePerMailbox(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)
	s.presenceCfg.SlotLease = time.Hour
	slotDo(t, s, "register", ident("m", "/x")) // team/worker
	slotDo(t, s, "register", ident("m", "/y")) // team/worker-2
	slotDo(t, s, "release", ident("m", "/x"))
	clock.add(10 * time.Minute)
	d := bossSend(t, s, "team/worker-3", "nobody") // never had a session: stays
	if d.Status != "pending" {
		t.Fatalf("mail to a never-attached slot: %+v", d)
	}
	if n := sweepNoSessionAt(t, s, clock, clock.now().Add(10*time.Minute)); n != 1 {
		t.Fatalf("notices: %d, want 1", n)
	}
	if got := noSessionNotices(t, s); len(got) != 1 || !strings.Contains(got[0], "team/worker-3 붙은 세션 없음") {
		t.Fatalf("notices %q", got)
	}
}

// withdrawMail posts /api/messages/{id}/withdraw as role.
func withdrawMail(s *Server, role string, id int64) (int, string) {
	w := callAs(s, "POST", "/api/messages/"+itoa(id)+"/withdraw", tokenFor(role), role, nil)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// Withdrawn mail and an unread directive the sender closed leave the
// recipient's mailbox: the next session of slot 1 does not get them, the
// pending alert leaves them out, and the sender's outbox shows withdrawn.
// Read mail cannot be withdrawn (409) and is only closed (#354).
func TestWithdrawAndCloseUnread(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)
	s.alerts = &insight.Thresholds{Pending: time.Hour}
	s.bossRoles = map[string]bool{"hq/boss": true, "team/lead": true}
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/lead','secret-team/lead','now')`); err != nil {
		t.Fatal(err)
	}
	slotDo(t, s, "register", ident("m", "/x")) // A, slot 1
	gone := bossSend(t, s, "team/worker", "withdraw me")
	closed := bossSend(t, s, "team/worker", "close me")
	read := bossSend(t, s, "team/worker", "already read")
	keep := bossSend(t, s, "team/worker", "keep")
	readAs(t, s, "team/worker", read.ID)

	if code, body := withdrawMail(s, "team/worker", gone.ID); code != 403 {
		t.Fatalf("withdraw by the recipient: %d %s", code, body)
	}
	if code, body := withdrawMail(s, "hq/boss", gone.ID); code != 200 || body != `{"id":`+itoa(gone.ID)+`,"withdrawn":true}` {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	if code, body := withdrawMail(s, "hq/boss", read.ID); code != 409 {
		t.Fatalf("withdraw read mail: %d %s, want 409", code, body)
	}
	if code, body := closeAs(s, "hq/boss", closed.ID); code != 200 || body != withdrawnJSON(closed.ID) {
		t.Fatalf("close unread: %d %s", code, body)
	}
	if code, body := closeAs(s, "hq/boss", read.ID); code != 200 || body != closedJSON(read.ID) {
		t.Fatalf("close read: %d %s", code, body)
	}
	if got := strings.Join(inboxSubjects(t, s, "team/worker"), ","); got != "keep,already read" {
		t.Fatalf("A's inbox: %s", got)
	}

	// A ends; B takes slot 1: "keep" is held for A, the withdrawn and closed
	// mail is gone; only the read mail stays in the mailbox, as before.
	clock.add(2 * time.Minute)
	slotDo(t, s, "register", ident("m", "/y"))
	if got := strings.Join(inboxSubjects(t, s, "team/worker"), ","); got != "already read" {
		t.Fatalf("B's inbox: %v", got)
	}
	if got := heldNotices(t, s, "hq/boss", "handoff-held"); len(got) != 1 || !strings.Contains(got[0], "#"+itoa(keep.ID)+" keep") {
		t.Fatalf("held notices: %q", got)
	}

	// Pending alert: a fresh unread mail and the withdrawn one; only the
	// fresh one is named.
	fresh := bossSend(t, s, "team/worker", "fresh")
	gone2 := bossSend(t, s, "team/worker", "gone too")
	if code, _ := withdrawMail(s, "hq/boss", gone2.ID); code != 200 {
		t.Fatalf("withdraw gone too: %d", code)
	}
	clock.add(2 * time.Hour)
	slotDo(t, s, "heartbeat", ident("m", "/y"))
	sweepOnce(t, s)
	var alert *wire.Message
	for _, m := range systemMail(t, s, "secret-team/lead", "team/lead") {
		if strings.HasPrefix(m.Subject, "[지표] pending team/worker") {
			m := m
			alert = &m
		}
	}
	if alert == nil || !strings.Contains(alert.Subject, "1통") || !strings.Contains(alert.Body, "#"+itoa(fresh.ID)+" hq/boss — fresh") ||
		strings.Contains(alert.Body, "gone too") || strings.Contains(alert.Body, "withdraw me") || strings.Contains(alert.Body, "close me") {
		t.Fatalf("pending alert: %+v", alert)
	}

	var out []wire.Message
	w := callAs(s, "GET", "/api/messages?box=outbox", tokenFor("hq/boss"), "hq/boss", nil)
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("outbox: %s", w.Body)
	}
	states := map[string]string{}
	for _, m := range out {
		states[m.Subject] = m.Status
	}
	if states["withdraw me"] != "withdrawn" || states["close me"] != "withdrawn" || states["already read"] != "read" {
		t.Fatalf("outbox states: %v", states)
	}
}
