package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// ambiguousSend sends from hq/boss to to and wants 409 ambiguous_recipient.
func ambiguousSend(t *testing.T, s *Server, path string, body map[string]any) wire.AmbiguousRecipient {
	t.Helper()
	w := callAs(s, "POST", path, tokenFor("hq/boss"), "hq/boss", body)
	var a wire.AmbiguousRecipient
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Error != wire.ErrAmbiguousRecipient {
		t.Fatalf("%s %v: %d %s", path, body, w.Code, w.Body)
	}
	return a
}

// keyedBossSend sends from hq/boss with an Idempotency-Key.
func keyedBossSend(s *Server, to, subject, key string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"recipient": to, "subject": subject})
	r := httptest.NewRequest("POST", "/api/messages", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+tokenFor("hq/boss"))
	r.Header.Set("X-Switchboard-Role", "hq/boss")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}

// A role's mail to a base name with two live slots is refused with the live
// slots; "<base>-1", a slot name, one live slot, an idempotent retry and an
// answer to slot 1's own mail go through (#364).
func TestSendToBaseWithTwoLiveSlotsIsRefused(t *testing.T) {
	t.Parallel()
	s, _ := slotsWithRoles(t)

	if m := bossSend(t, s, "team/worker", "no session"); m.Recipient != "team/worker" {
		t.Fatalf("no slot: %+v", m)
	}
	slotDo(t, s, "register", ident("m", "/x")) // team/worker-1
	if m := bossSend(t, s, "team/worker", "one live"); m.Recipient != "team/worker" {
		t.Fatalf("one live slot: %+v", m)
	}
	first := keyedBossSend(s, "team/worker", "keyed", "k1")
	if first.Code != 201 {
		t.Fatalf("keyed send: %d %s", first.Code, first.Body)
	}
	// slot 1 writes to the boss before a second session comes.
	w := callAs(s, "POST", "/api/messages", workerToken, "team/worker", map[string]any{"recipient": "hq/boss", "subject": "from slot 1"})
	var fromOne wire.Message
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &fromOne) != nil {
		t.Fatalf("slot 1 send: %d %s", w.Code, w.Body)
	}
	slotDo(t, s, "register", wire.SlotRequest{Machine: "m", Directory: "/y", Label: "review"}) // team/worker-2
	w = callAs(s, "POST", "/api/messages", workerToken, "team/worker-2", map[string]any{"recipient": "hq/boss", "subject": "from slot 2"})
	var fromTwo wire.Message
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &fromTwo) != nil {
		t.Fatalf("slot 2 send: %d %s", w.Code, w.Body)
	}

	a := ambiguousSend(t, s, "/api/messages", map[string]any{"recipient": "team/worker", "subject": "who"})
	want := []wire.LiveSlot{{Role: "team/worker-1", Machine: "m", Directory: "/x"}, {Role: "team/worker-2", Machine: "m", Directory: "/y", Label: "review"}}
	if a.Recipient != "team/worker" || len(a.LiveSlots) != 2 || a.LiveSlots[0] != want[0] || a.LiveSlots[1] != want[1] || !strings.Contains(a.Message, "team/worker-1") {
		t.Fatalf("refusal %+v", a)
	}
	if got := inboxSubjects(t, s, "team/worker"); strings.Contains(strings.Join(got, ","), "who") {
		t.Fatalf("refused mail stored: %v", got)
	}

	if m := bossSend(t, s, "team/worker-1", "slot 1 by name"); m.Recipient != "team/worker" {
		t.Fatalf("-1: %+v", m)
	}
	if m := bossSend(t, s, "team/worker-2", "slot 2 by name"); m.Recipient != "team/worker-2" {
		t.Fatalf("-2: %+v", m)
	}
	// The retry of a send stored before is answered as then.
	if again := keyedBossSend(s, "team/worker", "keyed", "k1"); again.Code != 200 {
		t.Fatalf("idempotent retry: %d %s", again.Code, again.Body)
	}
	// Answering slot 1's mail names slot 1 (its sender) and goes; naming the
	// base in an answer to slot 2's mail is refused.
	w = callAs(s, "POST", "/api/messages/"+itoa(fromOne.ID)+"/reply", tokenFor("hq/boss"), "hq/boss", map[string]any{"body": "ok"})
	if w.Code != 201 {
		t.Fatalf("reply to slot 1: %d %s", w.Code, w.Body)
	}
	ambiguousSend(t, s, "/api/messages/"+itoa(fromTwo.ID)+"/reply", map[string]any{"recipient": "team/worker", "body": "ok"})

	// System mail is not refused: it goes to slot 1 as before.
	if _, err := s.hostingAutoMail(hosting.AutoMailTo{Base: "team/worker"}, "system", "b", "sys:1"); err != nil {
		t.Fatal(err)
	}
	if got := inboxSubjects(t, s, "team/worker"); !strings.Contains(strings.Join(got, ","), "system") {
		t.Fatalf("system mail: %v", got)
	}
}

// A scheduled run's mail goes to the slot its hosted session holds, not to
// slot 1 held by another session; before that session registers it is held
// and delivered when it does, and completing it there counts for the run
// (#364).
func TestScheduledRunMailGoesToTheRecordsSlot(t *testing.T) {
	t.Parallel()
	s, url, clock := hostedSlotServer(t)
	r := newHostedRig(t, s, url, clock, "team/worker", workerToken, "/work/app")
	if g := r.plugin("register", "team/worker", "/other", ""); g.Role != "team/worker-1" {
		t.Fatalf("other session got %s", g.Role)
	}
	auto, err := s.hostingService().CreateAuto(r.id, wire.HostAutoCreate{Cron: "0 9 * * *", Prompt: "p", Label: "nightly"})
	if err != nil {
		t.Fatal(err)
	}

	// The hosted session has not registered yet: held for its folder.
	run, err := s.hostingHub().RunAuto(auto.ID)
	if err != nil || run.Result != wire.AutoFired || run.MailID == nil {
		t.Fatalf("run %+v %v", run, err)
	}
	if got := inboxSubjects(t, s, "team/worker"); strings.Contains(strings.Join(got, ","), "nightly") {
		t.Fatalf("slot 1 got the held run mail: %v", got)
	}
	// Another folder's heartbeat does not take it.
	r.plugin("heartbeat", "team/worker", "/other", "")
	if got := inboxSubjects(t, s, "team/worker"); strings.Contains(strings.Join(got, ","), "nightly") {
		t.Fatalf("slot 1 took the held run mail: %v", got)
	}
	g := r.plugin("register", "team/worker", "/work/app", r.record().AgentSessionID)
	if g.Role != "team/worker-2" {
		t.Fatalf("hosted session got %s", g.Role)
	}
	if got := inboxSubjects(t, s, "team/worker-2"); strings.Join(got, ",") != "nightly" {
		t.Fatalf("hosted slot inbox: %v", got)
	}
	if done, _ := mail.Completed(s.db, mail.CurrentID(s.db, *run.MailID)); done {
		t.Fatal("done before completed")
	}
	id := mail.CurrentID(s.db, *run.MailID)
	if w := callAs(s, "POST", "/api/messages/"+itoa(id)+"/complete", workerToken, "team/worker-2", nil); w.Code/100 != 2 {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	if done, err := mail.Completed(s.db, mail.CurrentID(s.db, *run.MailID)); !done || err != nil {
		t.Fatalf("completed via the old id: %v %v", done, err)
	}

	// Registered and live: the next run's mail goes straight to its slot.
	clock.add(time.Second) // another run time, another mail
	run, err = s.hostingHub().RunAuto(auto.ID)
	if err != nil || run.Result != wire.AutoFired || run.MailID == nil {
		t.Fatalf("second run %+v %v", run, err)
	}
	if got := inboxSubjects(t, s, "team/worker-2"); strings.Join(got, ",") != "nightly,nightly" {
		t.Fatalf("hosted slot inbox: %v", got)
	}
	if got := inboxSubjects(t, s, "team/worker"); strings.Contains(strings.Join(got, ","), "nightly") {
		t.Fatalf("slot 1 got run mail: %v", got)
	}
}
