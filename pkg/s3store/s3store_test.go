package s3store_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/s3store"
	"github.com/danielboothcloud/sundae/pkg/s3store/s3test"
)

func newStore(t *testing.T, mutate ...func(*config.S3SnapshotConfig)) (*s3store.Store, *s3test.Server) {
	t.Helper()
	srv := s3test.New(t)
	cfg := srv.Config(t, t.TempDir())
	for _, m := range mutate {
		m(&cfg)
	}
	st, err := s3store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st, srv
}

func TestSignedRequests(t *testing.T) {
	ctx := context.Background()
	st, srv := newStore(t, func(c *config.S3SnapshotConfig) { c.SSE = config.SSES3 })

	if _, err := st.Head(ctx, "a/missing"); !errors.Is(err, s3store.ErrNotFound) {
		t.Fatalf("head missing: %v", err)
	}
	if _, err := st.Get(ctx, "a/missing", 10); !errors.Is(err, s3store.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	// Keys with characters that need escaping sign and round-trip.
	key := "p/1/sb x+y=z~ü.json"
	if err := st.Put(ctx, key, []byte(`{"a":1}`), "application/json"); err != nil {
		t.Fatal(err)
	}
	o, ok := srv.Object(key)
	if !ok || string(o.Body) != `{"a":1}` || o.SSE != "AES256" || o.ContentType != "application/json" {
		t.Fatalf("stored %+v ok=%v", o, ok)
	}
	b, err := st.Get(ctx, key, 100)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("get: %q %v", b, err)
	}
	if _, err := st.Get(ctx, key, 3); err == nil || !strings.Contains(err.Error(), "larger than 3 bytes") {
		t.Fatalf("get bounded: %v", err)
	}
	h, err := st.Head(ctx, key)
	if err != nil || h.Size != 7 || h.LastModified.IsZero() || h.SSE != "AES256" {
		t.Fatalf("head: %+v %v", h, err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if _, ok := srv.Object(key); ok {
		t.Fatal("not deleted")
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	st, srv := newStore(t)
	srv.SetPageSize(2)
	for _, k := range []string{"p/1/a", "p/1/b", "p/1/c", "p/12/x", "p/2/a"} {
		srv.PutObject(k, []byte(k), nil)
	}
	objs, err := st.List(ctx, "p/1/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "p/1/a,p/1/b,p/1/c" || objs[0].Size != 5 || objs[0].LastModified.IsZero() {
		t.Fatalf("list = %+v", objs)
	}
	// Two pages were needed.
	lists := 0
	for _, r := range srv.Requests() {
		if r.Query.Get("list-type") == "2" {
			lists++
		}
	}
	if lists != 2 {
		t.Fatalf("lists = %d", lists)
	}
}

func TestPresignedPutAndGet(t *testing.T) {
	ctx := context.Background()
	st, srv := newStore(t, func(c *config.S3SnapshotConfig) { c.SSE = config.SSEKMS })
	body := bytes.Repeat([]byte("snapshot"), 1000)
	sum := md5.Sum(body) //nolint:gosec
	md := base64.StdEncoding.EncodeToString(sum[:])

	p, err := st.PresignPut(ctx, "sundae/sessions/1/sundae-alice-r.tar.gz", time.Hour, s3store.PutOptions{
		ContentMD5: md,
		Metadata:   map[string]string{"Sundae-Repo": "r"},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(p.URL)
	if u.Query().Get("X-Amz-Expires") != "3600" || !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "x-amz-server-side-encryption") {
		t.Fatalf("url = %s", p.URL)
	}
	if strings.Contains(p.URL, s3test.SecretAccessKey) {
		t.Fatal("secret in URL")
	}
	lines := p.HeaderLines()
	want := []string{"Content-Md5: " + md, "X-Amz-Meta-Sundae-Repo: r", "X-Amz-Server-Side-Encryption: aws:kms"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("headers = %q, want %q", lines, want)
	}

	put := func(body []byte, lines []string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, p.URL, bytes.NewReader(body))
		for _, l := range lines {
			k, v, _ := strings.Cut(l, ": ")
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		return resp.StatusCode
	}
	// Without the signed SSE header, or with another body, S3 refuses.
	if code := put(body, lines[:2]); code != http.StatusForbidden {
		t.Fatalf("unsigned-header put: %d", code)
	}
	if code := put(append(body, '!'), lines); code != http.StatusBadRequest {
		t.Fatalf("bad md5 put: %d", code)
	}
	if code := put(body, lines); code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}
	o, _ := srv.Object("sundae/sessions/1/sundae-alice-r.tar.gz")
	if !bytes.Equal(o.Body, body) || o.SSE != "aws:kms" || o.Metadata["sundae-repo"] != "r" {
		t.Fatalf("object = %+v", o)
	}

	g, err := st.PresignGet(ctx, "sundae/sessions/1/sundae-alice-r.tar.gz", time.Hour)
	if err != nil || len(g.HeaderLines()) != 0 {
		t.Fatalf("presign get: %v %v", g, err)
	}
	resp, err := get(ctx, g.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("get: %d %d bytes", resp.StatusCode, len(got))
	}

	// A presigned URL fits an exec argument, with room to spare.
	if len(p.URL) > 4096 {
		t.Fatalf("presigned URL is %d bytes", len(p.URL))
	}
}

func TestPresignedURLExpires(t *testing.T) {
	ctx := context.Background()
	signed := time.Now().Add(-2 * time.Hour)
	srv := s3test.New(t)
	st, err := s3store.New(srv.Config(t, t.TempDir()), s3store.WithClock(func() time.Time { return signed }))
	if err != nil {
		t.Fatal(err)
	}
	srv.PutObject("k", []byte("x"), nil)
	g, err := st.PresignGet(ctx, "k", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := get(ctx, g.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "Request has expired") {
		t.Fatalf("expired get: %d %s", resp.StatusCode, b)
	}
}

func TestWrongCredentials(t *testing.T) {
	ctx := context.Background()
	srv := s3test.New(t)
	cfg := srv.Config(t, t.TempDir())
	cfg.CredentialsFile = writeFile(t, "[default]\naws_access_key_id = "+s3test.AccessKeyID+"\naws_secret_access_key = wrong\n")
	bad, err := s3store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Head(ctx, "x"); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("bad secret head: %v", err)
	}
	if err := bad.Put(ctx, "x", []byte("y"), ""); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("bad secret put: %v", err)
	}
	// The credentials file is read for every request.
	cfg.CredentialsFile = "/nonexistent/creds"
	gone, _ := s3store.New(cfg)
	if _, err := gone.Head(ctx, "x"); err == nil || !strings.Contains(err.Error(), "credentials_file") {
		t.Fatalf("missing creds: %v", err)
	}
}

func TestInjectedFailure(t *testing.T) {
	ctx := context.Background()
	st, srv := newStore(t)
	srv.SetFail(func(r *http.Request, _ string) int {
		if r.Method == http.MethodPut {
			return http.StatusServiceUnavailable
		}
		return 0
	})
	err := st.Put(ctx, "k", []byte("v"), "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503 InjectedFailure") {
		t.Fatalf("put: %v", err)
	}
}

func TestAddressing(t *testing.T) {
	ctx := context.Background()
	creds := writeFile(t, "AWS_ACCESS_KEY_ID=AK\nAWS_SECRET_ACCESS_KEY=SK\n")
	cases := []struct {
		cfg  config.S3SnapshotConfig
		want string
	}{
		{config.S3SnapshotConfig{Region: "eu-west-2", Bucket: "b"}, "https://b.s3.eu-west-2.amazonaws.com/a/k.tar.gz?"},
		{config.S3SnapshotConfig{Region: "eu-west-2", Bucket: "b", PathStyle: true}, "https://s3.eu-west-2.amazonaws.com/b/a/k.tar.gz?"},
		{config.S3SnapshotConfig{Endpoint: "http://rgw.ceph:8080", Bucket: "b", PathStyle: true}, "http://rgw.ceph:8080/b/a/k.tar.gz?"},
		{config.S3SnapshotConfig{Endpoint: "https://minio.example.com", Bucket: "b"}, "https://b.minio.example.com/a/k.tar.gz?"},
	}
	for _, tc := range cases {
		tc.cfg.CredentialsFile = creds
		st, err := s3store.New(tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		p, err := st.PresignGet(ctx, "a/k.tar.gz", time.Minute)
		if err != nil || !strings.HasPrefix(p.URL, tc.want) {
			t.Errorf("%+v: url = %s (%v), want prefix %s", tc.cfg, p.URL, err, tc.want)
		}
		region := tc.cfg.Region
		if region == "" {
			region = "us-east-1"
		}
		if !strings.Contains(p.URL, "%2F"+region+"%2Fs3%2Faws4_request") {
			t.Errorf("url %s does not sign for %s", p.URL, region)
		}
	}
	if _, err := s3store.New(config.S3SnapshotConfig{Bucket: "b"}); err == nil {
		t.Fatal("AWS without a region")
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req) //nolint:wrapcheck
}
