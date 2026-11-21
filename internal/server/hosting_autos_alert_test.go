package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// alertMails are the subjects of the scheduled-run alert mails in role a's
// mailbox, oldest first (the runs' own mails have no colon).
func alertMails(t *testing.T, s *Server) []string {
	t.Helper()
	var out []string
	for _, m := range autoMails(t, s) {
		subject := strings.SplitN(strings.SplitN(m, " ", 2)[1], "|", 2)[0]
		if strings.Contains(subject, ":") {
			out = append(out, subject)
		}
	}
	return out
}

func discordLines(t *testing.T, s *Server, hook *webhookRecorder) []string {
	t.Helper()
	s.discord.Wait()
	var out []string
	for _, b := range hook.received() {
		var p struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Content)
	}
	return out
}

// error, timeout and overrun are told once while they go on (Discord and a
// mail to each notify role); a different one is told again; a run that ends
// fired or skipped after them tells "resolved" once (#336).
func TestHostAutoAlertsOnceAndResolved(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	hook := newWebhook(t)
	s.discord = insight.ConfigureDiscord(envOf(map[string]string{
		"SWITCHBOARD_DISCORD_WEBHOOK": hook.srv.URL + "/api/webhooks/1/secret",
		"SWITCHBOARD_PUBLIC_URL":      "https://mail.example",
	}))
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, `{"node":"pc","base":"a","profile":"team-a","directory":"/work/app","desired":"stopped"}`)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil || got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	fake := termtest.New()
	attachNode(t, base, token, fake)
	eventually(t, "stopped", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })
	got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"0 9 * * *","prompt":"<b>p</b>","label":"<i>nightly</i>","notify":["a"],"max_run_ms":600000}`)
	var auto wire.HostAuto
	if err := json.Unmarshal([]byte(got.body), &auto); err != nil || got.code != http.StatusCreated {
		t.Fatalf("auto add = %d %s", got.code, got.body)
	}
	alertOf := func() string {
		a, err := s.hostingService().GetAuto(auto.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a.Alert
	}
	run := func(want string) wire.HostAutoRun {
		t.Helper()
		makeDue(t, s, auto.ID)
		sweepAutos(t, s)
		r := lastAutoRun(t, s, auto.ID)
		if r.Result != want {
			t.Fatalf("run %+v, want %s", r, want)
		}
		return r
	}
	setSession := func(id string) {
		if _, err := s.db.Exec(`UPDATE host_autos SET session=? WHERE id=?`, id, auto.ID); err != nil {
			t.Fatal(err)
		}
	}

	// Two errors in a row: one alert.
	setSession("hs_gone")
	run(wire.AutoError)
	run(wire.AutoError)
	if a := alertOf(); a != wire.AutoError {
		t.Fatalf("alert = %q", a)
	}
	// A fired run that launched its session is not final yet: no "resolved".
	setSession(rec.ID)
	first := run(wire.AutoFired)
	if a := alertOf(); a != wire.AutoError {
		t.Fatalf("alert while the run holds its session = %q", a)
	}
	// It overruns: a different result, told again.
	if _, err := s.db.Exec(`UPDATE host_auto_runs SET launched_at=? WHERE id=?`, *first.LaunchedAt-600000, first.ID); err != nil {
		t.Fatal(err)
	}
	sweepAutos(t, s)
	if a := alertOf(); a != wire.AutoOverrun {
		t.Fatalf("alert after overrun = %q", a)
	}
	// The next run fires and its mail is completed: resolved, once.
	next := run(wire.AutoFired)
	completeMail(t, base, *next.MailID)
	sweepAutos(t, s)
	sweepAutos(t, s)
	if a := alertOf(); a != "" {
		t.Fatalf("alert after a clean run = %q", a)
	}

	link := " https://mail.example/#terminals/" + rec.ID + "/autos"
	want := []string{
		"예약 실행 " + itoa(auto.ID) + " · hs_gone · error https://mail.example/#terminals/hs_gone/autos",
		"예약 실행 " + itoa(auto.ID) + " · a · overrun" + link,
		"예약 실행 " + itoa(auto.ID) + " · a · 풀림 (overrun)" + link,
	}
	if got := discordLines(t, s, hook); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("discord %q, want %q", got, want)
	}
	wantMails := []string{
		"scheduled run error: <i>nightly</i>",
		"scheduled run overrun: <i>nightly</i>",
		"scheduled run resolved: <i>nightly</i> (was overrun)",
	}
	if got := alertMails(t, s); strings.Join(got, "\n") != strings.Join(wantMails, "\n") {
		t.Fatalf("alert mails %q, want %q", got, wantMails)
	}

	// Without notify roles only Discord is told.
	if _, err := s.db.Exec(`UPDATE host_autos SET notify='[]' WHERE id=?`, auto.ID); err != nil {
		t.Fatal(err)
	}
	setSession("hs_gone")
	run(wire.AutoError)
	if got := discordLines(t, s, hook); len(got) != 4 || len(alertMails(t, s)) != 3 {
		t.Fatalf("without notify: discord %q, mails %q", got, alertMails(t, s))
	}
}
