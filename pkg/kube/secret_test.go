package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI serves GET and POST for Secrets in one namespace.
type fakeAPI struct {
	mu       sync.Mutex
	secrets  map[string]Secret
	posts    int
	conflict *Secret // created by "someone else" right before a POST
	token    string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	const prefix = "/api/v1/namespaces/ns/secrets"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/"):
		s, ok := f.secrets[strings.TrimPrefix(r.URL.Path, prefix+"/")]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	case r.Method == http.MethodPost && r.URL.Path == prefix:
		f.posts++
		if f.conflict != nil {
			f.secrets[f.conflict.Metadata.Name] = *f.conflict
			f.conflict = nil
			http.Error(w, `{"message":"already exists"}`, http.StatusConflict)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var s Secret
		if err := json.Unmarshal(body, &s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.secrets[s.Metadata.Name] = s
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	default:
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}
}

func newFake(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{secrets: map[string]Secret{}, token: "t0ken"}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, &Client{BaseURL: srv.URL, Token: f.token, HTTP: srv.Client()}
}

func TestEnsureSecretCreatesOnce(t *testing.T) {
	f, c := newFake(t)
	tmpl := Secret{Metadata: ObjectMeta{Labels: map[string]string{"app": "sundae"}}}
	ctx := context.Background()

	created, err := c.EnsureSecret(ctx, "ns", "kek", "key-encryption-key", tmpl, func() ([]byte, error) { return RandomBase64(32) })
	if err != nil || !created {
		t.Fatalf("first EnsureSecret = %v, %v; want created", created, err)
	}
	s := f.secrets["kek"]
	if s.Kind != "Secret" || s.Metadata.Namespace != "ns" || s.Type != "Opaque" || s.Metadata.Labels["app"] != "sundae" {
		t.Fatalf("created secret = %+v", s)
	}
	raw, err := base64.StdEncoding.DecodeString(string(s.Data["key-encryption-key"]))
	if err != nil || len(raw) != 32 {
		t.Fatalf("value %q: decodes to %d bytes, err %v; want 32", s.Data["key-encryption-key"], len(raw), err)
	}

	created, err = c.EnsureSecret(ctx, "ns", "kek", "key-encryption-key", tmpl, func() ([]byte, error) { return []byte("other"), nil })
	if err != nil || created {
		t.Fatalf("second EnsureSecret = %v, %v; want kept", created, err)
	}
	if got := f.secrets["kek"].Data["key-encryption-key"]; string(got) != string(s.Data["key-encryption-key"]) {
		t.Fatalf("value changed to %q", got)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want 1", f.posts)
	}
}

func TestEnsureSecretMissingKey(t *testing.T) {
	f, c := newFake(t)
	f.secrets["kek"] = Secret{Metadata: ObjectMeta{Name: "kek"}, Data: map[string][]byte{"other": []byte("x")}}
	_, err := c.EnsureSecret(context.Background(), "ns", "kek", "key-encryption-key", Secret{}, func() ([]byte, error) { return []byte("v"), nil })
	if err == nil || !strings.Contains(err.Error(), `without key "key-encryption-key"`) {
		t.Fatalf("err = %v", err)
	}
	if f.posts != 0 {
		t.Fatalf("posts = %d, want 0", f.posts)
	}
}

func TestEnsureSecretConflict(t *testing.T) {
	f, c := newFake(t)
	f.conflict = &Secret{Metadata: ObjectMeta{Name: "kek"}, Data: map[string][]byte{"key-encryption-key": []byte("theirs")}}
	created, err := c.EnsureSecret(context.Background(), "ns", "kek", "key-encryption-key", Secret{}, func() ([]byte, error) { return []byte("mine"), nil })
	if err != nil || created {
		t.Fatalf("EnsureSecret = %v, %v; want kept theirs", created, err)
	}
	if got := string(f.secrets["kek"].Data["key-encryption-key"]); got != "theirs" {
		t.Fatalf("value = %q", got)
	}
}

func TestEnsureSecretAPIError(t *testing.T) {
	_, c := newFake(t)
	c.Token = "wrong"
	_, err := c.EnsureSecret(context.Background(), "ns", "kek", "k", Secret{}, func() ([]byte, error) { return []byte("v"), nil })
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v", err)
	}
}
