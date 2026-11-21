package wire

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// HostGrant is what a role may host under itself with its own token (#369):
// sessions of the listed base roles, on the listed nodes, in a directory
// under one of DirRoots. The admin sets it (host grant) and removes it (host
// revoke-grant); a role has at most one.
type HostGrant struct {
	Role      string   `json:"role"`
	Bases     []string `json:"bases"`
	Nodes     []string `json:"nodes"`
	DirRoots  []string `json:"dir_roots"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// Hosted session event kinds of the record itself (#369): a role added or
// removed it with its own token. The event's Actor is that role.
const (
	HostedEventAdd    = "add"
	HostedEventRemove = "remove"
)

var winDrivePattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// CleanDir is an absolute directory of either kind of node in one spelling,
// with "." and ".." resolved lexically (never above the root or volume): /…
// for Linux; C:\… or \\server\share\… for Windows, whose separators become
// \. Key is the spelling to compare: Windows paths are case-insensitive, so
// Key is lower case for them. ok is false for anything else.
func CleanDir(dir string) (clean, key string, ok bool) {
	switch {
	case strings.HasPrefix(dir, "/") && !strings.HasPrefix(dir, "//"):
		c := path.Clean(dir)
		return c, c, true
	case winDrivePattern.MatchString(dir):
		vol, rest := dir[:2], strings.ReplaceAll(dir[2:], `\`, "/")
		c := strings.ToUpper(vol[:1]) + vol[1:] + strings.ReplaceAll(path.Clean(rest), "/", `\`)
		return c, strings.ToLower(c), true
	case strings.HasPrefix(dir, `\\`) || strings.HasPrefix(dir, "//"):
		parts := strings.SplitN(strings.TrimLeft(strings.ReplaceAll(dir, "/", `\`), `\`), `\`, 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
			return "", "", false
		}
		rest := "/"
		if len(parts) == 3 {
			rest = path.Clean("/" + strings.ReplaceAll(parts[2], `\`, "/"))
		}
		c := `\\` + parts[0] + `\` + parts[1]
		if rest != "/" {
			c += strings.ReplaceAll(rest, "/", `\`)
		}
		return c, strings.ToLower(c), true
	}
	return "", "", false
}

// DirWithin reports whether dir is root or under it, after CleanDir on both:
// "/work/app" is within "/work", "/workshop" and "/work/../etc" are not, and
// on Windows "c:/Work/App" is within `C:\work`. It compares spellings only;
// a symbolic link is the node's to resolve (node.checkDirRoot).
func DirWithin(dir, root string) bool {
	_, d, ok1 := CleanDir(dir)
	_, r, ok2 := CleanDir(root)
	if !ok1 || !ok2 {
		return false
	}
	if d == r {
		return true
	}
	sep := "/"
	if !strings.HasPrefix(r, "/") {
		sep = `\`
	}
	if !strings.HasSuffix(r, sep) {
		r += sep
	}
	return strings.HasPrefix(d, r)
}

// RoleArgAllowed are the agent flags a role's hosted session may carry
// (#377), each with the shape its value must have. Every other flag is
// refused: the others widen folders, permissions or settings beyond the
// grant. The host checks a role's new record against it and the node checks
// the stored record again before it launches.
var RoleArgAllowed = map[string]*regexp.Regexp{
	"--model":  regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,63}$`),
	"--agent":  regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`),
	"--name":   regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:-]{0,63}$`),
	"--effort": regexp.MustCompile(`^[a-z]{1,16}$`),
}

// CheckRoleArgs returns an error naming the first arg a role's session may
// not carry. A flag takes its value as the next arg or after "=".
func CheckRoleArgs(args []string) error {
	names := make([]string, 0, len(RoleArgAllowed))
	for n := range RoleArgAllowed {
		names = append(names, n)
	}
	sort.Strings(names)
	for i := 0; i < len(args); i++ {
		name, val, inline := strings.Cut(args[i], "=")
		re, ok := RoleArgAllowed[name]
		if !ok {
			return fmt.Errorf("args: %s is not allowed for a role's session (allowed: %s)", args[i], strings.Join(names, ", "))
		}
		if !inline {
			if i+1 >= len(args) {
				return fmt.Errorf("args: %s needs a value", name)
			}
			i++
			val = args[i]
		}
		if !re.MatchString(val) {
			return fmt.Errorf("args: %s value %q is not allowed for a role's session (letters, digits and . _ - only)", name, val)
		}
	}
	return nil
}
