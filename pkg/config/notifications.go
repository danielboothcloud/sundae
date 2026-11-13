package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Notification triggers: the values notifications.on accepts.
const (
	// TriggerIdle notifies when a session's agent reports idle.
	TriggerIdle = "idle"
	// TriggerWorking notifies when a session's agent reports working.
	TriggerWorking = "working"
	// TriggerBlocked notifies when a session's agent is waiting for the user.
	TriggerBlocked = "blocked"
	// TriggerDone notifies when a session's agent finished a piece of work.
	TriggerDone = "done"
	// TriggerError notifies when a session's agent failed and stopped.
	TriggerError = "error"
	// TriggerSessionFailed notifies when starting or ending a session fails.
	TriggerSessionFailed = "session_failed"
	// TriggerForgeFailed notifies when a forge import, sync, push or
	// listing fails. Repeats of the same failure do not notify again until
	// the operation has succeeded in between.
	TriggerForgeFailed = "forge_failed"
)

// Triggers lists every valid notifications.on value.
var Triggers = []string{
	TriggerIdle, TriggerWorking, TriggerBlocked, TriggerDone, TriggerError,
	TriggerSessionFailed, TriggerForgeFailed,
}

// DefaultTriggers are the notifications.on default: the OSC 7501 states that
// need the user (blocked) or have a result for them (done, error).
var DefaultTriggers = []string{TriggerBlocked, TriggerDone, TriggerError}

// DefaultNotificationDebounce is the notifications.debounce default.
const DefaultNotificationDebounce = 30 * time.Second

// Click URL placeholders.
const (
	ClickRepo    = "{repo}"
	ClickSession = "{session}"
	ClickSandbox = "{sandbox}"
	ClickUser    = "{user}"
)

// clickPlaceholderRe matches anything in braces in a click_url.
var clickPlaceholderRe = regexp.MustCompile(`\{[^{}]*\}`)

// ntfyTopicRe matches a topic name ntfy accepts.
var ntfyTopicRe = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// NotificationsConfig configures notifications on session and forge events.
//
// While Ntfy.Enabled is false, the only notification sink is off and nothing
// in this section is validated beyond its YAML types.
type NotificationsConfig struct {
	// On lists what notifies (see Triggers). Defaults to DefaultTriggers.
	On []string `env:"ON" yaml:"on"`

	// Debounce is the per-session rate limit: after a notification, further
	// changes of that session are held for this long and only the latest is
	// sent, unless it repeats the notification already sent. 0 sends every
	// change immediately.
	Debounce time.Duration `env:"DEBOUNCE" yaml:"debounce"`

	// Ntfy publishes notifications to an ntfy server.
	Ntfy NtfyConfig `envPrefix:"NTFY_" yaml:"ntfy"`
}

// NtfyConfig configures the ntfy publisher.
type NtfyConfig struct {
	// Enabled turns the ntfy publisher on.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// URL is the ntfy server's base URL, e.g. https://ntfy.sh or a
	// self-hosted server.
	URL string `env:"URL" yaml:"url"`

	// Topic is the ntfy topic messages are published to.
	Topic string `env:"TOPIC" yaml:"topic"`

	// TokenFile is the path of a file holding an ntfy access token, sent as
	// a bearer token. It is read again for every publish, so a rotated token
	// is picked up without a restart. Optional.
	TokenFile string `env:"TOKEN_FILE" yaml:"token_file"`

	// ClickURL is opened when the notification is tapped. It may use the
	// placeholders {repo}, {session}, {sandbox} and {user}, which are
	// replaced by URL-escaped values. Optional.
	ClickURL string `env:"CLICK_URL" yaml:"click_url"`
}

// Token returns the ntfy access token: the contents of TokenFile with
// trailing whitespace trimmed, or "" when no token file is configured.
// TokenFile is read on every call.
func (n NtfyConfig) Token() (string, error) {
	if n.TokenFile == "" {
		return "", nil
	}
	token, err := readTokenFile(n.TokenFile)
	if err != nil {
		return "", fmt.Errorf("notifications.ntfy: %w", err)
	}
	return token, nil
}

// Notifies reports whether trigger is listed in On.
func (c NotificationsConfig) Notifies(trigger string) bool {
	return slices.Contains(c.On, trigger)
}

// ExpandClickURL fills the click URL placeholders. Values are URL-escaped,
// except that "/" in a repository name is kept. It returns "" when no click
// URL is configured, or when the URL uses a placeholder whose value is
// empty, e.g. {session} for a forge notification.
func (n NtfyConfig) ExpandClickURL(repo, session, sandbox, user string) string {
	if n.ClickURL == "" {
		return ""
	}
	values := map[string]string{
		ClickRepo:    repo,
		ClickSession: session,
		ClickSandbox: sandbox,
		ClickUser:    user,
	}
	missing := false
	out := clickPlaceholderRe.ReplaceAllStringFunc(n.ClickURL, func(p string) string {
		v, ok := values[p]
		if !ok || v == "" {
			missing = true
			return ""
		}
		return escapeClickValue(v)
	})
	if missing {
		return ""
	}
	return out
}

// escapeClickValue percent-encodes everything but unreserved characters and
// "/", so a value is safe in a path segment or a query value.
func escapeClickValue(v string) string {
	var b strings.Builder
	for _, c := range []byte(v) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// validateNotifications validates the notifications section. Nothing is
// checked while notifications.ntfy.enabled is false. Every problem is
// reported, each naming its key.
func (c *Config) validateNotifications() error {
	nc := &c.Notifications
	if !nc.Ntfy.Enabled {
		return nil
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("notifications."+format, args...))
	}

	if len(nc.On) == 0 {
		fail("on is empty: list at least one of %s, or set notifications.ntfy.enabled to false", strings.Join(Triggers, ", "))
	}
	on := make([]string, 0, len(nc.On))
	for i, t := range nc.On {
		t = strings.ToLower(strings.TrimSpace(t))
		switch {
		case !slices.Contains(Triggers, t):
			fail("on[%d] %q is not a trigger: use %s", i, nc.On[i], strings.Join(Triggers, ", "))
		case !slices.Contains(on, t):
			on = append(on, t)
		}
	}
	nc.On = on

	if nc.Debounce < 0 {
		fail("debounce %s must not be negative", nc.Debounce)
	}

	n := &nc.Ntfy
	n.URL = strings.TrimSuffix(strings.TrimSpace(n.URL), "/")
	if n.URL == "" {
		fail("ntfy.url is required when notifications.ntfy.enabled is true, e.g. https://ntfy.sh")
	} else if err := validateForgeURL(n.URL); err != nil {
		fail("ntfy.url %q: %v", n.URL, err)
	}

	n.Topic = strings.TrimSpace(n.Topic)
	if u, err := url.Parse(n.URL); err == nil && n.Topic != "" && strings.Trim(u.Path, "/") == n.Topic {
		// Messages are published as JSON to the server's root URL; a topic
		// URL would deliver the JSON as the message text.
		fail("ntfy.url %q must be the server's base URL, without the topic", n.URL)
	}
	switch {
	case n.Topic == "":
		fail("ntfy.topic is required when notifications.ntfy.enabled is true")
	case !ntfyTopicRe.MatchString(n.Topic):
		fail("ntfy.topic %q must be 1 to 64 letters, digits, '-' or '_'", n.Topic)
	}

	if n.TokenFile != "" {
		if !filepath.IsAbs(n.TokenFile) {
			n.TokenFile = filepath.Join(c.DataPath, n.TokenFile)
		}
		// Read the token once now so a missing or empty file fails at
		// startup. The value is not kept: Token reads the file again.
		if _, err := readTokenFile(n.TokenFile); err != nil {
			fail("ntfy.token_file: %v", err)
		}
	}

	n.ClickURL = strings.TrimSpace(n.ClickURL)
	if n.ClickURL != "" {
		if err := validateClickURL(n.ClickURL); err != nil {
			fail("ntfy.click_url %q: %v", n.ClickURL, err)
		}
	}

	return errors.Join(errs...)
}

// validateClickURL checks a click_url template: only known placeholders, and
// an absolute URL once they are filled in.
func validateClickURL(raw string) error {
	for _, p := range clickPlaceholderRe.FindAllString(raw, -1) {
		switch p {
		case ClickRepo, ClickSession, ClickSandbox, ClickUser:
		default:
			return fmt.Errorf("unknown placeholder %s: use %s, %s, %s or %s", p, ClickRepo, ClickSession, ClickSandbox, ClickUser)
		}
	}
	filled := NtfyConfig{ClickURL: raw}.ExpandClickURL("org/repo", "session", "sandbox", "user")
	if strings.ContainsAny(filled, "{}") {
		return errors.New("unbalanced braces")
	}
	u, err := url.Parse(filled)
	if err != nil {
		return err
	}
	if u.Scheme == "" {
		return errors.New("must be an absolute URL with a scheme, e.g. https://")
	}
	return nil
}

// notificationsEnviron returns the notifications section as
// SUNDAE_NOTIFICATIONS_* environment variables. The token itself is never
// included, only its file.
func (c *Config) notificationsEnviron() []string {
	nc := c.Notifications
	return []string{
		"SUNDAE_NOTIFICATIONS_ON=" + strings.Join(nc.On, ","),
		"SUNDAE_NOTIFICATIONS_DEBOUNCE=" + nc.Debounce.String(),
		fmt.Sprintf("SUNDAE_NOTIFICATIONS_NTFY_ENABLED=%t", nc.Ntfy.Enabled),
		"SUNDAE_NOTIFICATIONS_NTFY_URL=" + nc.Ntfy.URL,
		"SUNDAE_NOTIFICATIONS_NTFY_TOPIC=" + nc.Ntfy.Topic,
		"SUNDAE_NOTIFICATIONS_NTFY_TOKEN_FILE=" + nc.Ntfy.TokenFile,
		"SUNDAE_NOTIFICATIONS_NTFY_CLICK_URL=" + nc.Ntfy.ClickURL,
	}
}
