package cli

import (
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// The other-mailbox line follows the server's view_only field only: no
// name comparison, and nothing from a server that sends no field (#365).
func TestForeignFollowsViewOnly(t *testing.T) {
	own := wire.Message{ID: 7, Sender: "team/worker", Recipient: "team/boss"}
	if foreign(own) {
		t.Fatal("a reply without view_only (an older server) is not foreign")
	}
	own.ViewOnly = true
	if !foreign(own) {
		t.Fatal("view_only is foreign")
	}
	if foreign(wire.Message{ViewOnly: true}) {
		t.Fatal("an empty response is not foreign")
	}
}
