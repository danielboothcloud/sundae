package notify

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
)

func TestSanitize(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"plain":            {"Apply 3 to add?", "Apply 3 to add?"},
		"newlines and tab": {"line one\nline\ttwo\r\n", "line one line two"},
		"escape sequences": {"\x1b]0;evil\x07ok\x1b[31mred", "]0;evilok[31mred"},
		"c1 and del":       {"a\u0085b\u009bc\x7fd", "a bcd"},
		"bidi override":    {"good\u202Egnp.exe\u202C", "goodgnp.exe"},
		"zero width":       {"ad\u200Bmin\uFEFF", "admin"},
		"collapse spaces":  {"  a    b  ", "a b"},
		"invalid utf8":     {"a\xffb", "a\uFFFDb"},
		"unicode kept":     {"Déployé ✓ 日本", "Déployé ✓ 日本"},
	}
	for name, tc := range cases {
		if got := sanitize(tc.in, 1000); got != tc.want {
			t.Errorf("%s: sanitize(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	got := truncate(strings.Repeat("日", 100), 50)
	if len(got) > 50 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q (%d bytes)", got, len(got))
	}
}

func progress(p int) *int { return &p }

func TestStatusMessage(t *testing.T) {
	nc := config.NtfyConfig{ClickURL: "https://git.example.com/{repo}?session={session}&user={user}"}
	sess := events.Session{User: "alice", Repo: "infra", Name: "claude-1", Sandbox: "sundae-alice-infra"}

	m := statusMessage(events.SessionStatus{
		Session: sess,
		Record: events.Record{
			State: events.StateBlocked, Kind: events.BlockPermission, App: "claude-code",
			Progress: progress(40), Msg: "Apply 3 to add, 1 to change?",
		},
	}, nc)
	want := Message{
		Title:    "▲ blocked · infra / claude-1",
		Body:     "Needs permission: Apply 3 to add, 1 to change?\nclaude-code · 40%",
		Priority: priorityHigh,
		Tags:     []string{"blocked", "permission", "claude-code"},
		Click:    "https://git.example.com/infra?session=claude-1&user=alice",
	}
	if m.Title != want.Title || m.Body != want.Body || m.Priority != want.Priority || m.Click != want.Click || strings.Join(m.Tags, ",") != strings.Join(want.Tags, ",") {
		t.Fatalf("got  %+v\nwant %+v", m, want)
	}

	// done: default priority; the sandbox name stands in for a missing
	// session name; a child record's id follows it.
	m = statusMessage(events.SessionStatus{
		Session: events.Session{User: "alice", Repo: "infra", Sandbox: "sundae-alice-infra"},
		Record:  events.Record{ID: "build/test", State: events.StateDone, Msg: "Upgraded 12 packages"},
	}, config.NtfyConfig{})
	if m.Title != "✓ done · infra / sundae-alice-infra/build/test" || m.Body != "Upgraded 12 packages" || m.Priority != priorityDefault || m.Click != "" {
		t.Fatalf("done = %+v", m)
	}

	// No msg: the record title, else the app, else the state.
	m = statusMessage(events.SessionStatus{Session: sess, Record: events.Record{State: events.StateError, App: "cargo"}}, nc)
	if m.Body != "cargo" || m.Priority != priorityDefault {
		t.Fatalf("error = %+v", m)
	}
	m = statusMessage(events.SessionStatus{Session: sess, Record: events.Record{State: events.StateBlocked, Kind: events.BlockAuth}}, nc)
	if m.Body != "Needs a login" {
		t.Fatalf("blocked without msg = %+v", m)
	}
	m = statusMessage(events.SessionStatus{Session: sess, Record: events.Record{State: events.StateDone}}, nc)
	if m.Body != "Agent is done" {
		t.Fatalf("bare done = %+v", m)
	}
}

// Agent text is untrusted: no control characters, capped lengths, and tags
// limited to a safe character set.
func TestStatusMessageUntrusted(t *testing.T) {
	m := statusMessage(events.SessionStatus{
		Session: events.Session{User: "alice", Repo: "infra", Sandbox: "sb"},
		Record: events.Record{
			State: events.StateBlocked,
			Kind:  "permission\nX-Evil: 1",
			App:   "app with spaces",
			Title: strings.Repeat("t", 500),
			Msg:   "\x1b[2J**bold** [link](https://evil.example)\n" + strings.Repeat("m", 5000),
		},
	}, config.NtfyConfig{})
	for _, s := range []string{m.Title, m.Body} {
		for _, r := range s {
			if r < 0x20 && r != '\n' || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("control character %U in %q", r, s)
			}
		}
	}
	if strings.Contains(m.Body, "\x1b") || !strings.HasPrefix(m.Body, "[2J**bold**") {
		t.Fatalf("body = %q", m.Body[:40])
	}
	if len(m.Title) > maxTitle || len(m.Body) > maxBody || !utf8.ValidString(m.Body) {
		t.Fatalf("title %d bytes, body %d bytes", len(m.Title), len(m.Body))
	}
	if strings.Join(m.Tags, ",") != "blocked" {
		t.Fatalf("tags = %q", m.Tags)
	}
}

func TestFailureMessages(t *testing.T) {
	nc := config.NtfyConfig{ClickURL: "https://git.example.com/{repo}"}
	m := lifecycleFailedMessage(events.SessionLifecycle{
		Session: events.Session{User: "alice", Repo: "infra", Sandbox: "sundae-alice-infra"},
		Step:    events.LifecycleFailed, Op: "start", Reason: "openshell: git: exit status 128:\nfatal: boom",
	}, nc)
	if m.Title != "✗ session failed · infra / sundae-alice-infra" ||
		m.Body != "Could not start the session: openshell: git: exit status 128: fatal: boom" ||
		m.Priority != priorityDefault || m.Click != "https://git.example.com/infra" {
		t.Fatalf("lifecycle = %+v", m)
	}

	m = forgeFailedMessage(events.ForgeOutcome{Kind: "sync", Forge: "github", Repo: "github/me/x", Error: "401 Bad credentials"}, nc)
	if m.Title != "✗ forge sync failed · github · github/me/x" || m.Body != "401 Bad credentials" ||
		strings.Join(m.Tags, ",") != "forge,sync,github" || m.Click != "https://git.example.com/github/me/x" {
		t.Fatalf("forge = %+v", m)
	}
	// A discover failure has no repository, so a {repo} click URL is left
	// off.
	m = forgeFailedMessage(events.ForgeOutcome{Kind: "discover", Forge: "github", Error: "timeout"}, nc)
	if m.Title != "✗ forge discover failed · github" || m.Click != "" {
		t.Fatalf("discover = %+v", m)
	}
}
