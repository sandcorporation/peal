package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// SWITCHBOARD_HOSTING_TERMINAL and …_ALLOW (#367): where the browser
// terminal's ticket and socket answer.

const (
	loopbackPeerAddr = "127.0.0.1:40001"
	remotePeerAddr   = "192.0.2.7:40001" // TEST-NET-1: not loopback
)

// peerServer serves s with every request's peer address set to peer, as if
// the client connected from there.
func peerServer(t *testing.T, s *Server, peer string) string {
	t.Helper()
	h := s.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = peer
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// termTicket asks base for a ticket of mode; the ticket is "" when refused.
func (r *termRig) termTicket(t *testing.T, base, mode string, extra ...string) (string, int) {
	t.Helper()
	got := r.call(t, base, "POST", "/api/admin/terminals/"+r.id+"/ticket", `{"mode":"`+mode+`"}`, extra...)
	if got.code != http.StatusOK {
		return "", got.code
	}
	var tk wire.TerminalTicket
	if err := json.Unmarshal([]byte(got.body), &tk); err != nil || tk.Ticket == "" {
		t.Fatalf("ticket = %d %s", got.code, got.body)
	}
	return tk.Ticket, got.code
}

// termDial opens base's terminal socket as the rig's admin with origin.
func (r *termRig) termDial(t *testing.T, base, ticket, origin string, extra ...string) (*websocket.Conn, int) {
	t.Helper()
	h := http.Header{}
	for k, v := range r.hdr {
		h.Set(k, v)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		h.Set(extra[i], extra[i+1])
	}
	if r.cookie != nil {
		h.Set("Cookie", r.cookie.Name+"="+r.cookie.Value)
	}
	h.Set("Origin", origin)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(base, "http") + "/api/admin/terminals/" + r.id + "/socket?ticket=" + url.QueryEscape(ticket)
	c, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if res == nil {
			t.Fatalf("dial: %v", err)
		}
		return nil, res.StatusCode
	}
	t.Cleanup(func() { c.CloseNow() })
	c.SetReadLimit(16 << 20)
	return c, http.StatusSwitchingProtocols
}

// typeThrough opens an input socket at base and types text into the node.
func (r *termRig) typeThrough(t *testing.T, base, text string) *websocket.Conn {
	t.Helper()
	tk, code := r.termTicket(t, base, hosting.ModeInput)
	if code != http.StatusOK {
		t.Fatalf("input ticket = %d", code)
	}
	c, code := r.termDial(t, base, tk, base)
	if c == nil {
		t.Fatalf("input socket = %d", code)
	}
	before := string(r.fake.Typed(r.name()))
	typeInto(t, c, text)
	eventually(t, "the input typed", func() bool { return string(r.fake.Typed(r.name())) == before+text })
	return c
}

// meRefused is terminal_refused of base's /api/admin/me.
func (r *termRig) meRefused(t *testing.T, base string, extra ...string) string {
	t.Helper()
	got := r.call(t, base, "GET", "/api/admin/me", "", extra...)
	var me struct {
		TerminalRefused string `json:"terminal_refused"`
	}
	if err := json.Unmarshal([]byte(got.body), &me); err != nil || got.code != http.StatusOK {
		t.Fatalf("me = %d %s", got.code, got.body)
	}
	return me.TerminalRefused
}

func TestTerminalSettingByPeer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode               string // "" is unset: the default
		loopback, remote   int    // the ticket's status from each peer
		loopWhy, remoteWhy string // /api/admin/me terminal_refused
	}{
		{"", 200, 404, "", "loopback"},
		{config.TerminalLoopback, 200, 404, "", "loopback"},
		{config.TerminalOn, 200, 200, "", ""},
		{config.TerminalOff, 404, 404, "off", "off"},
	} {
		t.Run("mode="+c.mode, func(t *testing.T) {
			t.Parallel()
			r := newTermRigWith(t, hosting.Admin{}, func(s *Server) {
				var err error
				s.terminal, err = terminalSettings(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": c.mode}))
				if err != nil {
					t.Fatal(err)
				}
			})
			for _, p := range []struct {
				peer string
				want int
				why  string
			}{{loopbackPeerAddr, c.loopback, c.loopWhy}, {remotePeerAddr, c.remote, c.remoteWhy}} {
				base := peerServer(t, r.s, p.peer)
				if got := r.meRefused(t, base); got != p.why {
					t.Errorf("%s: terminal_refused %q, want %q", p.peer, got, p.why)
				}
				// The record list and the event log answer whatever the setting.
				if got := r.call(t, base, "GET", "/api/admin/terminals", ""); got.code != http.StatusOK {
					t.Errorf("%s: list = %d", p.peer, got.code)
				}
				if got := r.call(t, base, "GET", "/api/admin/terminals/"+r.id+"/events", ""); got.code != http.StatusOK {
					t.Errorf("%s: events = %d", p.peer, got.code)
				}
				tk, code := r.termTicket(t, base, hosting.ModeView)
				if code != p.want {
					t.Errorf("%s: ticket = %d, want %d", p.peer, code, p.want)
				}
				if p.want == http.StatusOK {
					if c, code := r.termDial(t, base, tk, base); c == nil {
						t.Errorf("%s: socket = %d", p.peer, code)
					} else {
						c.CloseNow()
					}
					r.typeThrough(t, base, "typed from "+p.peer+"\r").CloseNow()
					continue
				}
				// A refused address gets 404 on the socket too, even with a
				// ticket issued where the terminal answers.
				if tk, code = r.termTicket(t, r.base, hosting.ModeInput); code != http.StatusOK {
					tk = "x"
				}
				if _, code := r.termDial(t, base, tk, base); code != p.want {
					t.Errorf("%s: socket = %d, want %d", p.peer, code, p.want)
				}
			}
		})
	}
}

func TestTerminalLoopbackRefusesProxiedRequests(t *testing.T) {
	t.Parallel()
	r := newTermRig(t, hosting.Admin{}) // default: loopback; r.base's peer is loopback
	if _, code := r.termTicket(t, r.base, hosting.ModeView); code != http.StatusOK {
		t.Fatalf("direct ticket = %d", code)
	}
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if _, code := r.termTicket(t, r.base, hosting.ModeView, h, "198.51.100.9"); code != http.StatusNotFound {
			t.Errorf("ticket through a proxy (%s) = %d, want 404", h, code)
		}
		if got := r.meRefused(t, r.base, h, "198.51.100.9"); got != terminalLoopback {
			t.Errorf("%s: terminal_refused %q", h, got)
		}
	}
}

func TestTerminalAllowList(t *testing.T) {
	t.Parallel()
	allow := func(trust bool) func(*Server) {
		return func(s *Server) {
			s.terminal = terminalAccess{mode: config.TerminalOn, allow: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}}
			s.admin.TrustForwarded = trust
		}
	}
	t.Run("peer", func(t *testing.T) {
		t.Parallel()
		r := newTermRigWith(t, hosting.Admin{}, allow(false))
		out := peerServer(t, r.s, remotePeerAddr)
		if _, code := r.termTicket(t, out, hosting.ModeView); code != http.StatusForbidden {
			t.Errorf("ticket outside the list = %d, want 403", code)
		}
		if got := r.meRefused(t, out); got != terminalAllow {
			t.Errorf("terminal_refused %q", got)
		}
		tk, _ := r.termTicket(t, peerServer(t, r.s, "198.51.100.9:40001"), hosting.ModeView)
		if _, code := r.termDial(t, out, tk, out); code != http.StatusForbidden {
			t.Errorf("socket outside the list = %d, want 403", code)
		}
		// Without TRUST_FORWARDED the forwarded address is not believed.
		if _, code := r.termTicket(t, out, hosting.ModeView, "X-Forwarded-For", "198.51.100.9"); code != http.StatusForbidden {
			t.Errorf("forwarded address believed without TRUST_FORWARDED: %d", code)
		}
		in := peerServer(t, r.s, "198.51.100.9:40001")
		r.typeThrough(t, in, "inside\r").CloseNow()
	})
	t.Run("trust forwarded", func(t *testing.T) {
		t.Parallel()
		r := newTermRigWith(t, hosting.Admin{}, allow(true))
		base := peerServer(t, r.s, remotePeerAddr)
		// The last X-Forwarded-For entry (the nearest proxy's) counts.
		if _, code := r.termTicket(t, base, hosting.ModeView, "X-Forwarded-For", "203.0.113.5, 198.51.100.9"); code != http.StatusOK {
			t.Errorf("forwarded inside = %d, want 200", code)
		}
		if _, code := r.termTicket(t, base, hosting.ModeView, "X-Forwarded-For", "198.51.100.9, 203.0.113.5"); code != http.StatusForbidden {
			t.Errorf("forwarded outside = %d, want 403", code)
		}
		if _, code := r.termTicket(t, base, hosting.ModeView); code != http.StatusForbidden {
			t.Errorf("peer outside, nothing forwarded = %d, want 403", code)
		}
		tk, _ := r.termTicket(t, base, hosting.ModeInput, "X-Forwarded-For", "198.51.100.9")
		c, code := r.termDial(t, base, tk, base, "X-Forwarded-For", "198.51.100.9")
		if c == nil {
			t.Fatalf("socket forwarded inside = %d", code)
		}
		readFrame(t, c, websocket.MessageText) // the hello: attach is logged once it is sent
		c.CloseNow()
		eventually(t, "the detach row", func() bool { return len(terminalPeerLog(t, r.s, r.id)) >= 2 })
		if rows := terminalPeerLog(t, r.s, r.id); rows[0].ip != "198.51.100.9" {
			t.Errorf("logged address %+v, want the forwarded one", rows[0])
		}
	})
}

func TestTerminalSocketOriginAndTicket(t *testing.T) {
	t.Parallel()
	r := newTermRigWith(t, hosting.Admin{}, func(s *Server) { s.terminal.mode = config.TerminalOn })
	base := peerServer(t, r.s, remotePeerAddr)
	tk, _ := r.termTicket(t, base, hosting.ModeView)
	if _, code := r.termDial(t, base, tk, "http://other.example"); code != http.StatusForbidden {
		t.Errorf("socket with a foreign origin = %d, want 403", code)
	}
	c, code := r.termDial(t, base, tk, base)
	if c == nil {
		t.Fatalf("socket = %d", code)
	}
	c.CloseNow()
	if _, code := r.termDial(t, base, tk, base); code != http.StatusForbidden {
		t.Errorf("a used ticket = %d, want 403", code)
	}
}

func TestTerminalOnWithProxySecret(t *testing.T) {
	t.Parallel()
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if err := adminauth.Configure(&s.admin, fakeEnv(map[string]string{"AGENT_MAIL_ADMIN_PROXY_TOKEN": "proxy-secret"})); err != nil {
		t.Fatal(err)
	}
	r := &termRig{s: s, hdr: map[string]string{"X-Switchboard-Admin": "proxy-secret"}}
	r.start(t, hosting.Admin{}, func(s *Server) { s.terminal.mode = config.TerminalOn })
	base := peerServer(t, s, remotePeerAddr)
	r.typeThrough(t, base, "proxied\r").CloseNow()

	// Without the secret there is no admin.
	anon := &termRig{s: s, id: r.id}
	if _, code := anon.termTicket(t, base, hosting.ModeView); code != http.StatusUnauthorized {
		t.Errorf("ticket without the secret = %d, want 401", code)
	}
}

func TestTerminalInputSocketLog(t *testing.T) {
	t.Parallel()
	r := newTermRigWith(t, hosting.Admin{}, func(s *Server) { s.terminal.mode = config.TerminalOn })
	base := peerServer(t, r.s, remotePeerAddr)
	c := r.typeThrough(t, base, "do not keep me\r")
	c.Close(websocket.StatusNormalClosure, "")
	eventually(t, "the detach row", func() bool {
		rows := terminalPeerLog(t, r.s, r.id)
		return len(rows) > 0 && rows[len(rows)-1].kind == hosting.LogDetach
	})
	var kinds []string
	for _, l := range terminalPeerLog(t, r.s, r.id) {
		kinds = append(kinds, l.kind)
		if l.user != "alice" || l.mode != hosting.ModeInput || l.ip != "192.0.2.7" || l.at <= 0 {
			t.Errorf("row %+v: want alice, input, 192.0.2.7 and a time", l)
		}
	}
	if strings.Join(kinds, ",") != "attach,input,detach" {
		t.Errorf("kinds %v", kinds)
	}
	if hits := dbContains(t, r.s, "do not keep me"); len(hits) > 0 {
		t.Errorf("typed text stored in %v", hits)
	}
	got := r.call(t, base, "GET", "/api/admin/terminals/"+r.id+"/log", "")
	var log []wire.TerminalLogEntry
	if err := json.Unmarshal([]byte(got.body), &log); err != nil || len(log) != 3 {
		t.Fatalf("log = %d %s", got.code, got.body)
	}
	if e := log[0]; e.Kind != hosting.LogDetach || e.Mode != hosting.ModeInput || e.IP != "192.0.2.7" {
		t.Errorf("newest log entry %+v", e)
	}
}

type termPeerRow struct {
	user, kind, mode, ip string
	at                   int64
}

func terminalPeerLog(t *testing.T, s *Server, id string) []termPeerRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT admin_user,kind,mode,ip,at FROM terminal_log WHERE session=? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []termPeerRow
	for rows.Next() {
		var l termPeerRow
		if err := rows.Scan(&l.user, &l.kind, &l.mode, &l.ip, &l.at); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func TestHostingStartChecksTerminalSetting(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"SWITCHBOARD_HOSTING":    "on",
		"LISTEN_ADDR":            "127.0.0.1:8080",
		"DB_PATH":                "/srv/switchboard/test.db",
		"SWITCHBOARD_ADMIN_AUTH": "builtin",
	}
	with := func(kv ...string) func(string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return fakeEnv(m)
	}
	for _, c := range []struct {
		name string
		env  func(string) string
		want string // "" starts
	}{
		{"unset", with(), ""},
		{"loopback", with("SWITCHBOARD_HOSTING_TERMINAL", "loopback"), ""},
		{"on", with("SWITCHBOARD_HOSTING_TERMINAL", "on"), ""},
		{"off", with("SWITCHBOARD_HOSTING_TERMINAL", "off"), ""},
		{"bad value", with("SWITCHBOARD_HOSTING_TERMINAL", "yes"), `SWITCHBOARD_HOSTING_TERMINAL must be loopback, on or off (got "yes")`},
		{"bad allow", with("SWITCHBOARD_HOSTING_TERMINAL_ALLOW", "10.0.0.1/99"), `SWITCHBOARD_HOSTING_TERMINAL_ALLOW: "10.0.0.1/99" is not a CIDR range`},
		{"allow", with("SWITCHBOARD_HOSTING_TERMINAL_ALLOW", "198.51.100.0/24, 2001:db8::/32"), ""},
		{"on without sign-in", with("SWITCHBOARD_HOSTING_TERMINAL", "on", "SWITCHBOARD_ADMIN_AUTH", ""), "SWITCHBOARD_HOSTING_TERMINAL=on needs admin sign-in"},
	} {
		_, err := hostingStart(c.env)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}

	// The terminal's own check: on needs some admin sign-in, the proxy
	// secret included.
	for _, c := range []struct {
		env map[string]string
		ok  bool
	}{
		{map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": "on"}, false},
		{map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": "on", "SWITCHBOARD_ADMIN_PROXY_TOKEN": "proxy-secret"}, true},
		{map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": "on", "SWITCHBOARD_ADMIN_AUTH": "oidc"}, true},
		{map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": "off"}, false},
		{map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": "off", "SWITCHBOARD_ADMIN_AUTH": "builtin"}, true},
		{map[string]string{"SWITCHBOARD_HOSTING_TERMINAL": "loopback"}, true},
		{map[string]string{}, true},
	} {
		if err := terminalStart(fakeEnv(c.env)); (err == nil) != c.ok {
			t.Errorf("%v: err %v", c.env, err)
		}
	}
	if a, err := terminalSettings(fakeEnv(nil)); err != nil || a.mode != config.DefaultHostingTerminal || a.allow != nil {
		t.Errorf("unset = %+v %v", a, err)
	}
}
