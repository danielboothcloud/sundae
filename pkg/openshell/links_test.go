package openshell

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
)

// runLinks runs sharedLinksScript with a real sh, as the sandbox would,
// with mounts as the mount table.
func runLinks(t *testing.T, mounts []string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{"-c", sharedLinksScript, sharedLinksName}, args...)...)
	cmd.Env = append(os.Environ(), "SUNDAE_MOUNTINFO="+fakeMountinfo(t, mounts...))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	return string(out)
}

// fakeMountinfo writes a /proc/self/mountinfo with mount points mounts.
func fakeMountinfo(t *testing.T, mounts ...string) string {
	t.Helper()
	lines := "22 1 0:21 / / rw,relatime - overlay overlay rw\n"
	for i, m := range mounts {
		lines += fmt.Sprintf("%d 22 0:%d / %s rw,relatime - ceph 10.0.0.1:/vol rw\n", 30+i, 40+i, m)
	}
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// backups returns the contents of link's backups.
func backups(t *testing.T, link string) []string {
	t.Helper()
	names, err := filepath.Glob(link + ".sundae-bak.*")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range names {
		out = append(out, readFile(t, n))
	}
	return out
}

func readLink(t *testing.T, p string) string {
	t.Helper()
	got, err := os.Readlink(p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSharedLinksScript(t *testing.T) {
	td := t.TempDir()
	vol := filepath.Join(td, "vol")
	home := filepath.Join(td, "home 'a' \"b\"")
	if err := os.Mkdir(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	mounts := []string{vol}
	link := filepath.Join(home, ".config", "pi", "auth.json")
	target := filepath.Join(vol, "pi", "auth.json")

	// A login made before the volume existed moves onto it, keeping its mode.
	write(t, link, "old login")
	out := runLinks(t, mounts, link, vol, target)
	if !strings.Contains(out, "moved to the shared volume") || readLink(t, link) != target || readFile(t, link) != "old login" {
		t.Fatalf("first run: %q", out)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("target: %v %v", fi, err)
	}
	fi, err := os.Stat(filepath.Dir(target))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("target dir: %v %v", fi.Mode(), err)
	}
	if tmp, _ := filepath.Glob(target + ".sundae-*"); len(tmp) != 0 {
		t.Fatalf("left behind: %v", tmp)
	}

	// Idempotent: the correct link is left alone.
	before, _ := os.Lstat(link)
	if out := runLinks(t, mounts, link, vol, target); out != "" {
		t.Fatalf("second run: %q", out)
	}
	after, _ := os.Lstat(link)
	if !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) {
		t.Fatal("second run replaced the link")
	}

	// A new pod's stale local file is set aside, never over an older
	// backup; the volume's copy wins.
	for i, stale := range []string{"stale", "staler"} {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		write(t, link, stale)
		write(t, link+".sundae-bak", "older bak")
		out = runLinks(t, mounts, link, vol, target)
		if !strings.Contains(out, "Warning: link "+link+": the shared volume already has "+target+"; kept the local file as "+link+".sundae-bak.") ||
			readLink(t, link) != target || readFile(t, target) != "old login" || readFile(t, link+".sundae-bak") != "older bak" {
			t.Fatalf("stale file %d: %q", i, out)
		}
		if got := backups(t, link); len(got) != i+1 || !slices.Contains(got, stale) {
			t.Fatalf("backups = %q", got)
		}
	}

	// A link elsewhere is pointed at the target, saying so; a directory is skipped.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(dirLink, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A volume this pod does not mount gets no links, even if its directory exists.
	missing, unmounted := filepath.Join(td, "missing"), filepath.Join(td, "unmounted")
	if err := os.Mkdir(unmounted, 0o755); err != nil {
		t.Fatal(err)
	}
	out = runLinks(t, mounts,
		dirLink, vol, filepath.Join(vol, "claude"),
		link, vol, target,
		filepath.Join(home, "m"), missing, filepath.Join(missing, "m"),
		filepath.Join(home, "u"), unmounted, filepath.Join(unmounted, "d", "u"),
		filepath.Join(home, "new", "dir", "file"), vol, filepath.Join(vol, "new file"),
	)
	if !strings.Contains(out, "Warning: link "+dirLink+": is a directory") {
		t.Fatalf("directory: %q", out)
	}
	if !strings.Contains(out, "link "+link+": replaced a link to /elsewhere\n") || strings.Contains(out, "Warning: link "+link) {
		t.Fatalf("replaced link: %q", out)
	}
	for l, m := range map[string]string{"m": missing, "u": unmounted} {
		if !strings.Contains(out, "Warning: link "+filepath.Join(home, l)+": "+m+" is not mounted") {
			t.Fatalf("%s: %q", m, out)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing volume created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unmounted, "d")); !os.IsNotExist(err) {
		t.Fatalf("unmounted volume written: %v", err)
	}
	if fi, err := os.Lstat(dirLink); err != nil || !fi.IsDir() {
		t.Fatalf("directory touched: %v", err)
	}
	if readLink(t, link) != target {
		t.Fatal("wrong link not replaced")
	}
	// A dangling link is fine: the tool creates the file on login.
	if readLink(t, filepath.Join(home, "new", "dir", "file")) != filepath.Join(vol, "new file") {
		t.Fatal("new link")
	}

	// A file is not set aside for a directory target.
	fileLink, dirTarget := filepath.Join(home, "f"), filepath.Join(vol, "d")
	write(t, fileLink, "mine")
	write(t, filepath.Join(dirTarget, "x"), "x")
	out = runLinks(t, mounts, fileLink, vol, dirTarget)
	if !strings.Contains(out, "Warning: link "+fileLink+": is a file but "+dirTarget+" is a directory") || readFile(t, fileLink) != "mine" || len(backups(t, fileLink)) != 0 {
		t.Fatalf("file over directory: %q", out)
	}

	// A dangling symlink on the volume counts as no login: the local one replaces it.
	dangLink, dangTarget := filepath.Join(home, "g"), filepath.Join(vol, "g")
	write(t, dangLink, "local")
	if err := os.Symlink(filepath.Join(vol, "nowhere"), dangTarget); err != nil {
		t.Fatal(err)
	}
	out = runLinks(t, mounts, dangLink, vol, dangTarget)
	if fi, err := os.Lstat(dangTarget); err != nil || !fi.Mode().IsRegular() || readFile(t, dangTarget) != "local" || readLink(t, dangLink) != dangTarget {
		t.Fatalf("dangling target: %q %v", out, err)
	}
	if left, _ := filepath.Glob(dangTarget + ".sundae-*"); len(left) != 0 || len(backups(t, dangLink)) != 0 {
		t.Fatalf("left behind: %v", left)
	}

	// One failing link does not stop the others.
	if os.Getuid() != 0 {
		ro := filepath.Join(td, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		other := filepath.Join(home, "other")
		out = runLinks(t, mounts, filepath.Join(ro, "x", "y"), vol, filepath.Join(vol, "y"), other, vol, filepath.Join(vol, "other"))
		if !strings.Contains(out, "Warning: link "+filepath.Join(ro, "x", "y")+": ") || readLink(t, other) != filepath.Join(vol, "other") {
			t.Fatalf("failing link: %q", out)
		}
	}
}

// A link path that already is its target, through a linked parent
// directory, is left alone: linking it would replace the login with a loop.
func TestSharedLinksScriptLinkedParent(t *testing.T) {
	td := t.TempDir()
	vol := filepath.Join(td, "vol")
	mounts := []string{vol}
	target := filepath.Join(vol, "pi", "auth.json")
	write(t, target, "login")
	home := filepath.Join(td, "home")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(vol, "pi"), filepath.Join(home, ".config", "pi")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config", "pi", "auth.json")
	checkLogin := func(what string) {
		t.Helper()
		if fi, err := os.Lstat(target); err != nil || !fi.Mode().IsRegular() || readFile(t, target) != "login" {
			t.Fatalf("%s: target replaced: %v", what, err)
		}
		if left, _ := filepath.Glob(target + ".sundae-*"); len(left) != 0 {
			t.Fatalf("%s: left behind: %v", what, left)
		}
	}
	if out := runLinks(t, mounts, link, vol, target); out != "" {
		t.Fatalf("linked parent: %q", out)
	}
	checkLogin("linked parent")

	// Nested links: the directory link makes the file link's path its target.
	home2 := filepath.Join(td, "home2")
	dirLink := filepath.Join(home2, ".config", "pi")
	args := []string{dirLink, vol, filepath.Join(vol, "pi"), filepath.Join(dirLink, "auth.json"), vol, target}
	for i := range 2 {
		if out := runLinks(t, mounts, args...); i > 0 && out != "" || readLink(t, dirLink) != filepath.Join(vol, "pi") {
			t.Fatalf("nested %d: %q", i, out)
		}
		checkLogin("nested")
	}

	// What an earlier version left there, a link to itself, is reported.
	self := filepath.Join(vol, "pi", "self.json")
	if err := os.Symlink(self, self); err != nil {
		t.Fatal(err)
	}
	selfLink := filepath.Join(home, ".config", "pi", "self.json")
	if out := runLinks(t, mounts, selfLink, vol, self); !strings.Contains(out, "Warning: link "+selfLink+": is "+self+" itself") || readLink(t, self) != self {
		t.Fatalf("self link: %q", out)
	}

	// A link path inside its directory target is not linked either.
	inside := filepath.Join(home, ".config", "pi", "x")
	if out := runLinks(t, mounts, inside, vol, filepath.Join(vol, "pi")); !strings.Contains(out, "Warning: link "+inside+": is inside "+filepath.Join(vol, "pi")) {
		t.Fatalf("inside: %q", out)
	}
	if _, err := os.Lstat(filepath.Join(vol, "pi", "x")); !os.IsNotExist(err) {
		t.Fatalf("inside linked: %v", err)
	}
}

// Two sessions migrating their logins at once both keep them: one on the
// volume, the other set aside.
func TestSharedLinksScriptConcurrent(t *testing.T) {
	for i := range 10 {
		td := t.TempDir()
		vol := filepath.Join(td, "vol")
		target := filepath.Join(vol, "pi", "auth.json")
		links := []string{filepath.Join(td, "a", "auth.json"), filepath.Join(td, "b", "auth.json")}
		for _, l := range links {
			write(t, l, l)
		}
		var wg sync.WaitGroup
		outs := make([]string, len(links))
		for j, l := range links {
			wg.Go(func() { outs[j] = runLinks(t, []string{vol}, l, vol, target) })
		}
		wg.Wait()
		onVolume := readFile(t, target)
		for _, l := range links {
			if readLink(t, l) != target {
				t.Fatalf("run %d: %s not linked: %q", i, l, outs)
			}
			if l == onVolume {
				continue
			}
			if got := backups(t, l); !slices.Equal(got, []string{l}) {
				t.Fatalf("run %d: %s lost: volume has %q, backups %q, output %q", i, l, onVolume, got, outs)
			}
		}
		if !slices.Contains(links, onVolume) {
			t.Fatalf("run %d: volume has %q", i, onVolume)
		}
		if left, _ := filepath.Glob(target + ".sundae-*"); len(left) != 0 {
			t.Fatalf("run %d: left behind: %v", i, left)
		}
	}
}

func TestSharedLinkArgs(t *testing.T) {
	got := sharedLinkArgs([]config.SharedVolume{
		{Name: "a", MountPath: "/auth", Links: []config.SharedLink{{Path: "/h/x", Target: "pi/auth.json"}, {Path: "/h/y", Target: "y"}}},
		{Name: "b", MountPath: "/b"},
	})
	want := []string{"/h/x", "/auth", "/auth/pi/auth.json", "/h/y", "/auth", "/auth/y"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q", got)
	}
	if sharedLinkArgs([]config.SharedVolume{{Name: "b", MountPath: "/b"}}) != nil {
		t.Fatal("args without links")
	}
}

// TestSharedLinksPi checks that pi reads and logs out through the link,
// keeping it.
func TestSharedLinksPi(t *testing.T) {
	pi, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("pi is not installed")
	}
	td := t.TempDir()
	vol := filepath.Join(td, "vol")
	agent := filepath.Join(td, "agent")
	if err := os.Mkdir(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(agent, "auth.json")
	target := filepath.Join(vol, "pi", "auth.json")
	write(t, link, `{"openai":{"type":"api_key","key":"sk-sundae-test"}}`)
	runLinks(t, []string{vol}, link, vol, target)

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), pi, args...)
		cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+agent)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pi %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	if out := run("auth", "print-api-key", "--provider", "openai"); strings.TrimSpace(out) != "sk-sundae-test" {
		t.Fatalf("print-api-key = %q", out)
	}
	run("auth", "logout", "openai")
	if readLink(t, link) != target {
		t.Fatal("logout replaced the link")
	}
	if strings.Contains(readFile(t, target), "sk-sundae-test") {
		t.Fatal("logout did not reach the volume")
	}
}
