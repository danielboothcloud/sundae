package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
)

// request is one request the fake ntfy server received.
type request struct {
	method, path, auth, contentType string
	body                            map[string]any
	raw                             string
}

// fakeNtfy is an httptest ntfy server. Each request is answered by the next
// status in statuses (200 once they run out) and recorded.
type fakeNtfy struct {
	srv      *httptest.Server
	mu       sync.Mutex
	statuses []int
	headers  map[string]string
	reqs     []request
	got      chan request
	block    chan struct{}
}

func newFakeNtfy(t *testing.T, statuses ...int) *fakeNtfy {
	t.Helper()
	f := &fakeNtfy{statuses: statuses, got: make(chan request, 100), headers: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := request{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), raw: string(raw)}
		_ = json.Unmarshal(raw, &req.body)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		status := http.StatusOK
		if len(f.statuses) > 0 {
			status, f.statuses = f.statuses[0], f.statuses[1:]
		}
		for k, v := range f.headers {
			w.Header().Set(k, v)
		}
		block := f.block
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
			}
			return
		}
		if status == http.StatusFound {
			w.Header().Set("Location", f.srv.URL+"/elsewhere")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"x"}`))
		f.got <- req
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNtfy) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

// testPublisher returns a publisher for f whose waits are recorded instead
// of slept, on a clock the waits advance.
func testPublisher(f *fakeNtfy, cfg config.NtfyConfig) (*ntfyPublisher, *[]time.Duration) {
	cfg.URL = f.srv.URL
	if cfg.Topic == "" {
		cfg.Topic = "agents"
	}
	p := newNtfyPublisher(cfg, f.srv.Client())
	now := t0
	var waits []time.Duration
	p.now = func() time.Time { return now }
	p.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		return nil
	}
	return p, &waits
}

func TestNtfyPublishFormat(t *testing.T) {
	f := newFakeNtfy(t)
	td := t.TempDir()
	tokenFile := filepath.Join(td, "token")
	if err := os.WriteFile(tokenFile, []byte("tk_first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := testPublisher(f, config.NtfyConfig{Topic: "agents", TokenFile: tokenFile})

	m := Message{
		Title:    "▲ blocked · infra / claude-1",
		Body:     "Needs permission: Apply?\nclaude-code",
		Priority: priorityHigh,
		Tags:     []string{"blocked", "permission"},
		Click:    "https://git.example.com/infra",
	}
	if err := p.Publish(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	// JSON publish goes to the server root, never to a topic URL.
	if r.method != http.MethodPost || r.path != "/" || r.contentType != "application/json" || r.auth != "Bearer tk_first" {
		t.Fatalf("request = %+v", r)
	}
	want := map[string]any{
		"topic":    "agents",
		"title":    "▲ blocked · infra / claude-1",
		"message":  "Needs permission: Apply?\nclaude-code",
		"priority": float64(4),
		"tags":     []any{"blocked", "permission"},
		"click":    "https://git.example.com/infra",
		"markdown": false,
	}
	if len(r.body) != len(want) {
		t.Fatalf("body = %s", r.raw)
	}
	for k, v := range want {
		got, _ := json.Marshal(r.body[k])
		exp, _ := json.Marshal(v)
		if string(got) != string(exp) {
			t.Errorf("%s = %s, want %s", k, got, exp)
		}
	}

	// The token file is read for every publish: a rotated token is used.
	if err := os.WriteFile(tokenFile, []byte("tk_second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	reqs = f.requests()
	if reqs[1].auth != "Bearer tk_second" {
		t.Fatalf("auth after rotation = %q", reqs[1].auth)
	}
	// Optional fields are left out; markdown is always sent as false.
	if _, ok := reqs[1].body["click"]; ok {
		t.Fatalf("empty click sent: %s", reqs[1].raw)
	}
	if reqs[1].body["markdown"] != false {
		t.Fatalf("markdown = %v", reqs[1].body["markdown"])
	}
}

func TestNtfyNoToken(t *testing.T) {
	f := newFakeNtfy(t)
	p, _ := testPublisher(f, config.NtfyConfig{})
	if err := p.Publish(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if auth := f.requests()[0].auth; auth != "" {
		t.Fatalf("auth = %q", auth)
	}
}

func TestNtfyRetries(t *testing.T) {
	t.Run("5xx then success, with backoff", func(t *testing.T) {
		f := newFakeNtfy(t, 500, 502, 200)
		p, waits := testPublisher(f, config.NtfyConfig{})
		if err := p.Publish(context.Background(), Message{Body: "b"}); err != nil {
			t.Fatal(err)
		}
		if n := len(f.requests()); n != 3 {
			t.Fatalf("attempts = %d", n)
		}
		if len(*waits) != 2 || (*waits)[0] != time.Second || (*waits)[1] != 2*time.Second {
			t.Fatalf("waits = %v", *waits)
		}
	})

	t.Run("429 honours Retry-After", func(t *testing.T) {
		f := newFakeNtfy(t, 429, 200)
		f.headers["Retry-After"] = "7"
		p, waits := testPublisher(f, config.NtfyConfig{})
		if err := p.Publish(context.Background(), Message{Body: "b"}); err != nil {
			t.Fatal(err)
		}
		if len(*waits) != 1 || (*waits)[0] != 7*time.Second {
			t.Fatalf("waits = %v", *waits)
		}
	})

	t.Run("retrying is bounded in time", func(t *testing.T) {
		f := newFakeNtfy(t, 500, 500, 500, 500, 500, 500, 500, 500, 500, 500, 500, 500)
		p, waits := testPublisher(f, config.NtfyConfig{})
		p.retryFor = 20 * time.Second
		err := p.Publish(context.Background(), Message{Body: "b"})
		if err == nil || !strings.Contains(err.Error(), "giving up after 5 attempts") || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v", err)
		}
		var total time.Duration
		for _, w := range *waits {
			total += w
		}
		if total > 20*time.Second {
			t.Fatalf("waited %v", total)
		}
	})

	for _, status := range []int{400, 401, 403, http.StatusFound} {
		t.Run("no retry on "+http.StatusText(status), func(t *testing.T) {
			f := newFakeNtfy(t, status)
			p, waits := testPublisher(f, config.NtfyConfig{})
			err := p.Publish(context.Background(), Message{Body: "b"})
			var perm permanentError
			if !errors.As(err, &perm) || len(*waits) != 0 || len(f.requests()) != 1 {
				t.Fatalf("err = %v, waits = %v, requests = %d", err, *waits, len(f.requests()))
			}
		})
	}

	t.Run("missing token file is retried, then reported", func(t *testing.T) {
		f := newFakeNtfy(t)
		p, _ := testPublisher(f, config.NtfyConfig{TokenFile: filepath.Join(t.TempDir(), "gone")})
		p.retryFor = 3 * time.Second
		err := p.Publish(context.Background(), Message{Body: "b"})
		if err == nil || !strings.Contains(err.Error(), "token_file") || len(f.requests()) != 0 {
			t.Fatalf("err = %v, requests = %d", err, len(f.requests()))
		}
	})
}

func TestNtfyTimeout(t *testing.T) {
	f := newFakeNtfy(t)
	f.block = make(chan struct{})
	defer close(f.block)
	p, waits := testPublisher(f, config.NtfyConfig{})
	p.attemptTimeout = 50 * time.Millisecond
	p.retryFor = 1500 * time.Millisecond // room for two attempts

	start := time.Now()
	err := p.Publish(context.Background(), Message{Body: "b"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "giving up after 2 attempts") {
		t.Fatalf("err = %v", err)
	}
	if len(f.requests()) != 2 || len(*waits) != 1 {
		t.Fatalf("requests = %d, waits = %v", len(f.requests()), *waits)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

func TestNtfyCancel(t *testing.T) {
	f := newFakeNtfy(t, 500, 500)
	p, _ := testPublisher(f, config.NtfyConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	p.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if err := p.Publish(ctx, Message{Body: "b"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}
