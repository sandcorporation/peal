package wire

import "testing"

func TestDirWithin(t *testing.T) {
	for _, c := range []struct {
		dir, root string
		want      bool
	}{
		{"/work", "/work", true},
		{"/work/app", "/work", true},
		{"/work/app/", "/work/", true},
		{"/work/./app", "/work", true},
		{"/work/a/../b", "/work", true},
		{"/workshop", "/work", false},
		{"/work/../etc", "/work", false},
		{"/work/app/../../etc", "/work", false},
		{"/../work/app", "/work", true},
		{"/etc", "/", true},
		{"work/app", "/work", false},
		{"//server/share/work", "/work", false},
		{`C:\Work\App`, `c:\work`, true},
		{"c:/work/app", `C:\Work`, true},
		{`C:\Work\..\Windows`, `C:\Work`, false},
		{`C:\Workshop`, `C:\Work`, false},
		{`D:\Work\app`, `C:\Work`, false},
		{`C:\..\..\Work\app`, `C:\Work`, true},
		{`C:\anything`, `C:\`, true},
		{`\\Server\Share\Work\app`, `\\server\share\work`, true},
		{"//server/share/work/app", `\\server\share\work`, true},
		{`\\server\share\..\other\work`, `\\server\share\work`, false},
		{`\\server\other\work`, `\\server\share`, false},
		{`\\server\..\work`, `\\server\share`, false},
		{"/work/app", `C:\work`, false},
		{`C:\work\app`, "/work", false},
	} {
		if got := DirWithin(c.dir, c.root); got != c.want {
			t.Errorf("DirWithin(%q, %q) = %v, want %v", c.dir, c.root, got, c.want)
		}
	}
}

func TestCleanDirKeepsCaseAndUsesBackslashOnWindows(t *testing.T) {
	for in, want := range map[string]string{
		"c:/Work/./App/":     `C:\Work\App`,
		`\\srv\Share\a\..\b`: `\\srv\Share\b`,
		"/work//app/.":       "/work/app",
	} {
		if got, _, ok := CleanDir(in); !ok || got != want {
			t.Errorf("CleanDir(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}
