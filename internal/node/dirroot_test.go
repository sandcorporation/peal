package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDirRootResolvesSymbolicLinks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "work")
	inside := filepath.Join(root, "app")
	outside := filepath.Join(tmp, "secret")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkDirRoot(Record{Directory: inside, DirRoot: root}); err != nil {
		t.Errorf("a directory under its root: %v", err)
	}
	if err := checkDirRoot(Record{Directory: outside}); err != nil {
		t.Errorf("an admin's record (no root): %v", err)
	}
	if err := checkDirRoot(Record{Directory: filepath.Join(root, "missing"), DirRoot: root}); err == nil {
		t.Error("a missing directory passed")
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := checkDirRoot(Record{Directory: escape, DirRoot: root}); err == nil || !strings.Contains(err.Error(), "outside its grant root") {
		t.Errorf("a link out of the root = %v", err)
	}
	// A root reached through a link still holds its own directories.
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := checkDirRoot(Record{Directory: filepath.Join(alias, "app"), DirRoot: root}); err != nil {
		t.Errorf("the root through a link: %v", err)
	}
}

func TestCheckRoleProfileNeedsTheBaseRole(t *testing.T) {
	t.Parallel()
	dir, legacy := t.TempDir(), t.TempDir()
	write := func(d, name, role string) {
		if err := os.WriteFile(filepath.Join(d, name+".json"), []byte(`{"endpoint":"http://mail.example","role":"`+role+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(dir, "pc-worker", "team/worker")
	write(dir, "pc-boss", "team/boss")
	write(legacy, "old-worker", "team/worker-2")
	dirs := []string{dir, legacy}
	rec := func(profile string) Record {
		return Record{ID: "hs_00000000000000000000000000000001", Base: "team/worker", Profile: profile, DirRoot: "/work"}
	}
	for _, p := range []string{"pc-worker", "old-worker", "hosted-hs_00000000000000000000000000000001"} {
		if err := checkRoleProfile(rec(p), dirs); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := checkRoleProfile(rec("pc-boss"), dirs); err == nil || !strings.Contains(err.Error(), "is role team/boss, not the session's base team/worker") {
		t.Errorf("another role's profile = %v", err)
	}
	if err := checkRoleProfile(rec("missing"), dirs); err == nil {
		t.Error("a missing profile passed")
	}
	admin := rec("pc-boss")
	admin.DirRoot = ""
	if err := checkRoleProfile(admin, dirs); err != nil {
		t.Errorf("the admin's record: %v", err)
	}
}

func TestCheckRoleArgsHoldsOnlyRoleRecords(t *testing.T) {
	t.Parallel()
	role := func(args ...string) Record { return Record{DirRoot: "/work", Args: args} }
	if err := checkRoleArgs(role("--model", "claude-x", "--effort=high")); err != nil {
		t.Errorf("allowed args: %v", err)
	}
	for _, a := range [][]string{{"--add-dir", "/"}, {"--settings=x"}, {"--model", "../x"}, {"--model"}} {
		if err := checkRoleArgs(role(a...)); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
	if err := checkRoleArgs(Record{Args: []string{"--add-dir", "/"}}); err != nil {
		t.Errorf("an admin record is not held to the allow-list: %v", err)
	}
}
