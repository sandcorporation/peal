package hosting

import (
	"log"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// AutoNotice is one alert of a scheduled run (#336): Run ended with Result
// error, timeout or overrun (Resolved false), or a run after such an alert
// ended fired or skipped (Resolved true; Was is the alerted result).
type AutoNotice struct {
	Auto     wire.HostAuto
	Role     string // the hosted session's role; "" when the session is gone
	Run      wire.HostAutoRun
	Resolved bool
	Was      string
}

// AutoAlert delivers a notice: the server posts it to Discord and mails
// Auto.Notify. hosting imports neither insight nor mail.
type AutoAlert func(n AutoNotice)

// SetAutoAlert installs the alert function of scheduled runs. Without it
// results are recorded but nobody is told.
func (h *Hub) SetAutoAlert(fn AutoAlert) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.autoAlert = fn
}

// autoAlerting is whether result is one that is alerted; autoResolving is
// whether it resolves an alert. Other results (busy, waiting, node-offline)
// leave the alert as it is.
func autoAlerting(result string) bool {
	return result == wire.AutoError || result == wire.AutoTimeout || result == wire.AutoOverrun
}

func autoResolving(result string) bool {
	return result == wire.AutoFired || result == wire.AutoSkipped
}

// autoRunFinal reports whether run's result is final when it is recorded: a
// fired run that launched its session holds it, and its result is final
// only when the host stops watching it (it may become overrun).
func autoRunFinal(run wire.HostAutoRun) bool {
	return !(run.Launched && run.Result == wire.AutoFired && run.EndedAt == nil)
}

// alertAuto applies run's final result to its scheduled run's alert: an
// alerted result different from the one standing alerts once (stored in
// host_autos.alert, so a restart keeps it), and a resolving result after an
// alert sends "resolved". The store is changed only when it still holds the
// alert read, so two runs racing alert once.
func (h *Hub) alertAuto(run wire.HostAutoRun) {
	if !autoAlerting(run.Result) && !autoResolving(run.Result) {
		return
	}
	a, err := h.svc.GetAuto(run.Auto)
	if err != nil {
		return // removed meanwhile
	}
	n := AutoNotice{Auto: a, Run: run}
	switch {
	case autoAlerting(run.Result) && a.Alert != run.Result:
		n.Auto.Alert = run.Result
	case autoResolving(run.Result) && a.Alert != "":
		n.Resolved, n.Was, n.Auto.Alert = true, a.Alert, ""
	default:
		return
	}
	changed, err := h.svc.swapAutoAlert(a.ID, a.Alert, n.Auto.Alert)
	if err != nil || !changed {
		if err != nil {
			log.Printf("hosting: scheduled run %d: alert: %v", a.ID, err)
		}
		return
	}
	if rec, err := h.svc.Get(a.Session); err == nil {
		n.Role = rec.Base
	}
	h.mu.Lock()
	fn := h.autoAlert
	h.mu.Unlock()
	if fn != nil {
		fn(n)
	}
}

// swapAutoAlert sets host_autos.alert of id to to when it is still from.
func (s *Service) swapAutoAlert(id int64, from, to string) (bool, error) {
	res, err := s.db.Exec(`UPDATE host_autos SET alert=? WHERE id=? AND alert=?`, to, id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
