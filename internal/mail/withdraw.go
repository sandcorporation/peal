package mail

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Withdrawn mail (#354).
//
// A sender takes back mail its recipient has not read: the row stays, with
// status "withdrawn", withdrawn_at, addressed_to keeping where it was going,
// and recipient "withdrawn:" (no principal can be named with ':'), so it
// leaves every view keyed on the recipient: inbox, read, watch, events and
// polls, pending metrics and alerts, the [미수신] sweep, handoffs and held
// label delivery; no_reply is set, so it is no open directive. The sender's outbox shows it with status withdrawn. Mail
// the recipient read, acknowledged, completed or replied to cannot be withdrawn
// (errAlreadyRead): close it or send new mail instead.

const (
	statusWithdrawn = "withdrawn"
	withdrawnBox    = "withdrawn:"
)

var errAlreadyRead = errors.New("the recipient already read this message")

// withdraw takes back message id from its recipient in tx. The caller has
// checked that the caller sent it. It returns the mailbox it left (to wake)
// or errAlreadyRead.
func withdraw(tx *sql.Tx, id int64, now time.Time) (string, error) {
	var recipient, status, state string
	err := tx.QueryRow(`SELECT m.recipient,m.status,COALESCE(rs.status,'pending') FROM messages m `+StatusJoin+` WHERE m.id=?`, id).Scan(&recipient, &status, &state)
	if err != nil {
		return "", err
	}
	if status == statusWithdrawn {
		return "", nil // again: the same answer
	}
	if state != "pending" || (status != "pending" && status != statusHeld) {
		return "", errAlreadyRead
	}
	// A reply from the recipient means it was seen, read mark or not.
	var replied int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE reply_to=? AND sender=?`, id, recipient).Scan(&replied); err != nil {
		return "", err
	}
	if replied > 0 {
		return "", errAlreadyRead
	}
	if _, err = tx.Exec(`UPDATE messages SET recipient=?,status=?,addressed_to=COALESCE(addressed_to,?),withdrawn_at=?,no_reply=1 WHERE id=?`,
		withdrawnBox, statusWithdrawn, recipient, now.UTC().Format(time.RFC3339Nano), id); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`DELETE FROM role_states WHERE message_id=? AND role=?`, id, recipient); err != nil {
		return "", err
	}
	if status != "pending" {
		return "", nil // held: in no mailbox
	}
	return recipient, nil
}

// Withdraw serves POST /api/messages/{id}/withdraw for role, the sender:
// 404 for no such message, 403 for a caller that is not the sender, 400 for
// system mail, 409 when the recipient already read it.
func (s *Service) Withdraw(w http.ResponseWriter, r *http.Request, role string, id int64) {
	m, err := s.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if m.Sender != role {
		http.Error(w, "only the sender may withdraw a message", http.StatusForbidden)
		return
	}
	var left string
	err = store.InTx(r.Context(), s.db, func(tx *sql.Tx) error {
		var e error
		left, e = withdraw(tx, id, s.now())
		return e
	})
	if errors.Is(err, errAlreadyRead) {
		http.Error(w, err.Error()+": close it or send new mail", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if left != "" {
		s.hub.Publish(left)
	}
	writeJSON(w, wire.Withdrawn{ID: id, Withdrawn: true})
}
