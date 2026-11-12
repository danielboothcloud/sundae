package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/version"
)

// Retry defaults for publishing to ntfy.
const (
	// DefaultAttemptTimeout bounds one HTTP request.
	DefaultAttemptTimeout = 10 * time.Second
	// DefaultRetryFor bounds the time spent retrying one message.
	DefaultRetryFor = 2 * time.Minute
	// defaultBackoff is the first wait between attempts; it doubles up to
	// maxBackoff.
	defaultBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// ntfyMessage is ntfy's JSON publish format (https://docs.ntfy.sh/publish/,
// "Publish as JSON"), posted to the server's root URL. Publishing as JSON
// rather than with headers carries UTF-8 titles without header encoding and
// keeps message text out of HTTP headers.
type ntfyMessage struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
	// Markdown is always false: agent text is never rendered as markup.
	Markdown bool `json:"markdown"`
}

// ntfyPublisher publishes messages to one ntfy topic.
type ntfyPublisher struct {
	cfg            config.NtfyConfig
	client         *http.Client
	attemptTimeout time.Duration
	retryFor       time.Duration
	backoff        time.Duration
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
}

func newNtfyPublisher(cfg config.NtfyConfig, client *http.Client) *ntfyPublisher {
	if client == nil {
		client = &http.Client{}
	}
	// A redirect would resend the message, and the token, somewhere else.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ntfyPublisher{
		cfg:            cfg,
		client:         &c,
		attemptTimeout: DefaultAttemptTimeout,
		retryFor:       DefaultRetryFor,
		backoff:        defaultBackoff,
		now:            time.Now,
		sleep:          sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// permanentError is a failure retrying cannot fix, e.g. a rejected token.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Publish sends m, retrying network errors, timeouts, 429 and 5xx responses
// with exponential backoff for up to retryFor. Other responses fail at once.
// It returns when the message is accepted, retrying is exhausted, or ctx is
// done.
func (p *ntfyPublisher) Publish(ctx context.Context, m Message) error {
	body, err := json.Marshal(ntfyMessage{
		Topic:    p.cfg.Topic,
		Title:    m.Title,
		Message:  m.Body,
		Priority: m.Priority,
		Tags:     m.Tags,
		Click:    m.Click,
	})
	if err != nil {
		return err
	}

	deadline := p.now().Add(p.retryFor)
	wait := p.backoff
	for attempt := 1; ; attempt++ {
		retryAfter, err := p.attempt(ctx, body)
		if err == nil {
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) || ctx.Err() != nil {
			return err
		}
		if retryAfter > wait {
			wait = min(retryAfter, maxBackoff)
		}
		if p.now().Add(wait).After(deadline) {
			return fmt.Errorf("giving up after %d attempts: %w", attempt, err)
		}
		if err := p.sleep(ctx, wait); err != nil {
			return fmt.Errorf("after %d attempts: %w", attempt, errors.Join(err, ctx.Err()))
		}
		wait = min(wait*2, maxBackoff)
	}
}

// attempt makes one request. It returns the server's Retry-After, if any,
// with a retryable error.
func (p *ntfyPublisher) attempt(ctx context.Context, body []byte) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL+"/", bytes.NewReader(body))
	if err != nil {
		return 0, permanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	ua := "soft-serve"
	if version.Version != "" {
		ua += "/" + version.Version
	}
	req.Header.Set("User-Agent", ua)
	// The token file is read for every attempt, so a rotated token is used
	// as soon as it lands.
	token, err := p.cfg.Token()
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0, nil
	}

	err = fmt.Errorf("ntfy: %s: %s", resp.Status, sanitize(string(snippet), 200))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		var after time.Duration
		if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
			after = time.Duration(s) * time.Second
		}
		return after, err
	}
	return 0, permanentError{err}
}
