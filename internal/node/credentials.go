package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/ownerfile"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Profiles keeps the role credentials a node got from its host (#343) as
// CLI profiles: <Dir>/hosted-<record id>.json, readable by the node's user
// only (mode 0600, a protected user-only DACL on Windows), with the host's
// endpoint, the record's base role, the token and the credential id. A
// record's session runs with SWITCHBOARD_PROFILE naming that file. Dir is the
// client config directory the CLI reads profiles from (ClientConfigDir), so
// the session's CLI finds it.
type Profiles struct {
	Dir      string
	Endpoint string
	// Logf, when set, gets one line for a profile that could not be removed.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	pending map[string]bool // records whose profile removal failed and is retried
	running bool            // a retry goroutine is alive
	remove  func(string) error
	retry   time.Duration
}

// removeRetryEvery is how often a failed profile removal is tried again.
const removeRetryEvery = time.Second

func (p *Profiles) removeFile(path string) error {
	if p.remove != nil {
		return p.remove(path)
	}
	return os.Remove(path)
}

// hostedProfile is the profile file: the CLI reads endpoint, role and token
// and ignores credential.
type hostedProfile struct {
	Endpoint   string `json:"endpoint"`
	Role       string `json:"role"`
	Token      string `json:"token"`
	Credential string `json:"credential"`
}

var hostedProfileFile = regexp.MustCompile(`^hosted-(hs_[0-9a-f]{32})\.json$`)

// ClientConfigDir is where the CLI keeps its profiles: SWITCHBOARD_CONFIG_DIR,
// else <user config>/switchboard.
func ClientConfigDir() (string, error) {
	if d := config.EnvAlias("CONFIG_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, config.ConfigDirName), nil
}

func (p *Profiles) path(record string) string {
	return filepath.Join(p.Dir, wire.HostedProfileName(record)+".json")
}

func (p *Profiles) read(record string) (hostedProfile, bool) {
	var v hostedProfile
	b, err := os.ReadFile(p.path(record))
	if err != nil || json.Unmarshal(b, &v) != nil || v.Credential == "" || v.Token == "" {
		return hostedProfile{}, false
	}
	return v, true
}

// Has reports whether the node holds a credential for record.
func (p *Profiles) Has(record string) bool {
	_, ok := p.read(record)
	return ok
}

// records lists the record ids that have a profile file.
func (p *Profiles) records() []string {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if m := hostedProfileFile.FindStringSubmatch(e.Name()); m != nil && !e.IsDir() {
			out = append(out, m[1])
		}
	}
	return out
}

// Header is the value of config.NodeCredentialsHeaderName: the credential
// ids held, comma separated, or "-" for none.
func (p *Profiles) Header() string {
	var ids []string
	for _, r := range p.records() {
		if v, ok := p.read(r); ok {
			ids = append(ids, v.Credential)
		}
	}
	if len(ids) == 0 {
		return "-"
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// Write stores c as record's profile, replacing an older one. The file is
// made user-only before the token is written, then renamed into place.
func (p *Profiles) Write(record string, c wire.NodeCredential) error {
	if !hostedProfileFile.MatchString(wire.HostedProfileName(record)+".json") || c.ID == "" || c.Token == "" || c.Role == "" {
		return errors.New("node: credential: bad record id or empty credential")
	}
	b, err := json.MarshalIndent(hostedProfile{Endpoint: p.Endpoint, Role: c.Role, Token: c.Token, Credential: c.ID}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	// A rewrite cancels a pending removal of the older profile, and the
	// retry cannot delete the new one while it is written.
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, record)
	dst := p.path(record)
	tmp := dst + ".new"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tokenFileMode)
	if err != nil {
		return err
	}
	if err = ownerfile.RestrictToUser(tmp); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("node: credential profile %s: %w", dst, err)
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// apply points each record the node holds a credential for at its profile
// and removes the profiles of records no longer assigned. A removal that
// fails (on Windows another process may hold the file open) is logged and
// retried in the background, so the assignment is not held up.
func (p *Profiles) apply(recs []Record) {
	keep := map[string]bool{}
	for i := range recs {
		keep[recs[i].ID] = true
		if p.Has(recs[i].ID) {
			recs[i].Profile = wire.HostedProfileName(recs[i].ID)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for r := range keep {
		delete(p.pending, r)
	}
	for _, r := range p.records() {
		if !keep[r] {
			p.tryRemoveLocked(r, true)
		}
	}
	p.startRetryLocked()
}

// tryRemoveLocked removes record's profile; a failure other than "already
// gone" marks it pending. Holds p.mu.
func (p *Profiles) tryRemoveLocked(record string, log bool) {
	err := p.removeFile(p.path(record))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		delete(p.pending, record)
		return
	}
	if log && p.Logf != nil {
		p.Logf("node: remove profile of %s: %v (retrying)", record, err)
	}
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	p.pending[record] = true
}

// startRetryLocked starts the retry goroutine when removals are pending and
// none is running. It ends once nothing is pending. Holds p.mu.
func (p *Profiles) startRetryLocked() {
	if len(p.pending) == 0 || p.running {
		return
	}
	p.running = true
	every := p.retry
	if every <= 0 {
		every = removeRetryEvery
	}
	go func() {
		for {
			time.Sleep(every)
			p.mu.Lock()
			for r := range p.pending {
				p.tryRemoveLocked(r, false)
			}
			if len(p.pending) == 0 {
				p.running = false
				p.mu.Unlock()
				return
			}
			p.mu.Unlock()
		}
	}()
}
