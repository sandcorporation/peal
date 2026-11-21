package cli

import (
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// The lines a send prints about the receiving mailbox (#354).
func TestRecipientNotes(t *testing.T) {
	cases := []struct {
		name string
		m    wire.Message
		want string
	}{
		{"live", wire.Message{NoSession: true, RecipientState: &wire.RecipientState{Mailbox: "team/worker", Live: true}}, ""},
		{"ended and held", wire.Message{RecipientState: &wire.RecipientState{Mailbox: "team/worker-2", EndedS: 7*60 + 30, Held: true}},
			"받는 세션 없음: team/worker-2, 마지막 세션 7분 전 끝남. 붙잡아 둠"},
		{"slot 1 empty, others live", wire.Message{NoSession: false, RecipientState: &wire.RecipientState{Mailbox: "team/worker", LiveSlots: []string{"team/worker-2", "team/worker-3"}}},
			"team/worker-2, team/worker-3 이 살아 있음. 그 슬롯 이름으로 보내라"},
		{"held, others live", wire.Message{RecipientState: &wire.RecipientState{Mailbox: "team/worker", EndedS: 60, Held: true, LiveSlots: []string{"team/worker-2"}}},
			"받는 세션 없음: team/worker, 마지막 세션 1분 전 끝남. 붙잡아 둠|team/worker-2 이 살아 있음. 그 슬롯 이름으로 보내라"},
		{"never attached", wire.Message{NoSession: true, RecipientState: &wire.RecipientState{Mailbox: "team/worker"}},
			"no session of team/worker is attached; the mail waits in its inbox"},
		{"server before #354", wire.Message{NoSession: true}, "no session of team/worker is attached; the mail waits in its inbox"},
	}
	for _, c := range cases {
		if got := strings.Join(RecipientNotes("team/worker", c.m), "|"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A 409 ambiguous_recipient answer prints the choice on stderr, the body on
// stdout with --json, and exits 1; other answers are not taken (#364).
func TestRefusedAmbiguous(t *testing.T) {
	body := []byte(`{"error":"ambiguous_recipient","message":"m","recipient":"team/worker","live_slots":[{"role":"team/worker-1","machine":"m","directory":"/x"},{"role":"team/worker-2","machine":"m","directory":"/y","label":"review"}]}`)
	var out, errOut strings.Builder
	code, ok := refusedAmbiguous(409, body, false, &out, &errOut)
	want := "보내지 않음: team/worker 에 산 슬롯이 2개다. 슬롯 이름으로 골라라 (1번은 team/worker-1)\n  team/worker-1  m  /x\n  team/worker-2  m  /y  라벨 review\n"
	if !ok || code != ExitError || out.String() != "" || errOut.String() != want {
		t.Fatalf("text: %v %d %q %q", ok, code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code, ok = refusedAmbiguous(409, body, true, &out, &errOut); !ok || code != ExitError || !strings.Contains(out.String(), `"live_slots"`) || errOut.String() != want {
		t.Fatalf("json: %v %d %q %q", ok, code, out.String(), errOut.String())
	}
	for _, c := range []struct {
		status int
		body   string
	}{{409, "unread mail: close it or send new mail"}, {403, string(body)}, {409, `{"error":"other"}`}} {
		if _, ok := refusedAmbiguous(c.status, []byte(c.body), false, &out, &errOut); ok {
			t.Fatalf("took %d %s", c.status, c.body)
		}
	}
}
