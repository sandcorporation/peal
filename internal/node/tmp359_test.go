package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTmp359OpenHandleRemove(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.json")
	os.WriteFile(f, []byte("x"), 0o600)
	h, _ := os.Open(f)
	err := os.Remove(f)
	t.Logf("REMOVE_WHILE_OPEN err=%v", err)
	h.Close()
}
