// Package s3test is an in-memory S3 server for tests. It serves one bucket,
// path-style, and checks every request's AWS Signature Version 4 itself,
// header-signed or presigned, independently of the signer Soft Serve uses:
// a request whose signature, payload hash, Content-MD5 or expiry does not
// hold is refused as S3 would refuse it.
package s3test

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // Content-MD5 is MD5.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// Credentials the server accepts.
const (
	AccessKeyID     = "AKIDTESTSNAPSHOTS"
	SecretAccessKey = "c2VjcmV0LXNuYXBzaG90LWtleS1mb3ItdGVzdHM"
	Region          = "us-test-1"
	Bucket          = "snapshots"
)

// Object is a stored object.
type Object struct {
	Body         []byte
	Metadata     map[string]string
	SSE          string
	ContentType  string
	LastModified time.Time
}

// Request records a request the server answered.
type Request struct {
	Method    string
	Key       string
	Query     url.Values
	Presigned bool
	Status    int
	// Header is the request's header.
	Header http.Header
}

// Server is the fake S3 server.
type Server struct {
	*httptest.Server
	t   testing.TB
	now func() time.Time

	mu      sync.Mutex
	objects map[string]*Object
	reqs    []Request
	// fail, when set, may answer a request with a status of its choosing
	// (non-zero) instead of serving it.
	fail func(r *http.Request, key string) int
	// pageSize bounds a LIST page; 0 is 1000.
	pageSize int
}

// New starts a server, closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{t: t, now: time.Now, objects: map[string]*Object{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Config returns a validated snapshots.s3 configuration for the server,
// with the credentials file in dir.
func (s *Server) Config(t testing.TB, dir string) config.S3SnapshotConfig {
	t.Helper()
	p := filepath.Join(dir, "s3-credentials")
	creds := fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", AccessKeyID, SecretAccessKey)
	if err := os.WriteFile(p, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.S3SnapshotConfig{
		Enabled:         true,
		Endpoint:        s.URL,
		Region:          Region,
		Bucket:          Bucket,
		Prefix:          config.DefaultSnapshotPrefix,
		CredentialsFile: p,
		PathStyle:       true,
		On:              []string{config.SnapshotOnEnd},
	}
}

// SetFail sets a function that can fail requests; nil serves all.
func (s *Server) SetFail(f func(r *http.Request, key string) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = f
}

// SetPageSize bounds LIST pages.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// SetClock sets the server's clock, used for presigned URL expiry.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Object returns a copy of the object at key.
func (s *Server) Object(key string) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return Object{}, false
	}
	c := *o
	c.Body = slices.Clone(o.Body)
	return c, true
}

// Keys returns the stored keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// PutObject stores an object directly.
func (s *Server) PutObject(key string, body []byte, meta map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = &Object{Body: slices.Clone(body), Metadata: meta, LastModified: s.now().UTC()}
}

// DeleteObject removes an object directly.
func (s *Server) DeleteObject(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
}

// Requests returns the requests served so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func (s *Server) record(r *http.Request, key string, presigned bool, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Key: key, Query: r.URL.Query(), Presigned: presigned, Status: status, Header: r.Header.Clone()})
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>%s</Message></Error>", code, xmlEscape(msg)) //nolint:errcheck
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	presigned := r.URL.Query().Get("X-Amz-Signature") != ""
	status := s.handle(w, r, bucket, key, body, presigned)
	s.record(r, key, presigned, status)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, bucket, key string, body []byte, presigned bool) int {
	if bucket != Bucket {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return http.StatusNotFound
	}
	if code, msg := s.verify(r, body, presigned); code != "" {
		writeError(w, http.StatusForbidden, code, msg)
		return http.StatusForbidden
	}
	if r.Header.Get("Transfer-Encoding") == "chunked" || slices.Contains(r.TransferEncoding, "chunked") {
		// AWS S3 refuses a PUT without a Content-Length.
		writeError(w, http.StatusNotImplemented, "NotImplemented", "A header you provided implies functionality that is not implemented")
		return http.StatusNotImplemented
	}
	s.mu.Lock()
	fail, now, pageSize := s.fail, s.now, s.pageSize
	s.mu.Unlock()
	if fail != nil {
		if code := fail(r, key); code != 0 {
			writeError(w, code, "InjectedFailure", "injected by the test")
			return code
		}
	}

	switch {
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		return s.list(w, r, pageSize)
	case key == "":
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket operation")
		return http.StatusNotImplemented
	}

	switch r.Method {
	case http.MethodPut:
		if md := r.Header.Get("Content-Md5"); md != "" {
			sum := md5.Sum(body) //nolint:gosec
			if base64.StdEncoding.EncodeToString(sum[:]) != md {
				writeError(w, http.StatusBadRequest, "BadDigest", "The Content-MD5 you specified did not match what was received.")
				return http.StatusBadRequest
			}
		}
		if int64(len(body)) > 5<<30 {
			writeError(w, http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.")
			return http.StatusBadRequest
		}
		o := &Object{Body: body, Metadata: map[string]string{}, SSE: r.Header.Get("X-Amz-Server-Side-Encryption"), ContentType: r.Header.Get("Content-Type"), LastModified: now().UTC().Truncate(time.Second)}
		for k, vs := range r.Header {
			if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok {
				o.Metadata[name] = vs[0]
			}
		}
		s.mu.Lock()
		s.objects[key] = o
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+hex.EncodeToString(md5sum(body))+`"`)
		w.WriteHeader(http.StatusOK)
		return http.StatusOK
	case http.MethodGet, http.MethodHead:
		o, ok := s.Object(key)
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
			} else {
				writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			}
			return http.StatusNotFound
		}
		for k, v := range o.Metadata {
			w.Header().Set("X-Amz-Meta-"+k, v)
		}
		if o.SSE != "" {
			w.Header().Set("X-Amz-Server-Side-Encryption", o.SSE)
		}
		w.Header().Set("Last-Modified", o.LastModified.Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(o.Body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(o.Body) //nolint:errcheck,gosec
		}
		return http.StatusOK
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent
	}
	writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	return http.StatusMethodNotAllowed
}

func md5sum(b []byte) []byte {
	sum := md5.Sum(b) //nolint:gosec
	return sum[:]
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, pageSize int) int {
	if pageSize <= 0 {
		pageSize = 1000
	}
	q := r.URL.Query()
	prefix, after := q.Get("prefix"), q.Get("continuation-token")
	s.mu.Lock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > pageSize
	if truncated {
		keys = keys[:pageSize]
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, k := range keys {
		o := s.objects[k]
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>", xmlEscape(k), len(o.Body), o.LastModified.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "<IsTruncated>%t</IsTruncated>", truncated)
	if truncated {
		fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", xmlEscape(keys[len(keys)-1]))
	}
	b.WriteString("</ListBucketResult>")
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, b.String()) //nolint:errcheck,gosec
	return http.StatusOK
}

// verify checks the request's SigV4 signature and returns an S3 error code
// and message when it does not hold.
func (s *Server) verify(r *http.Request, body []byte, presigned bool) (code, msg string) {
	q := r.URL.Query()
	var credential, signedHeaders, signature, amzDate, payloadHash string
	if presigned {
		if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
			return "AuthorizationQueryParametersError", "X-Amz-Algorithm must be AWS4-HMAC-SHA256"
		}
		credential, signedHeaders, signature, amzDate = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		payloadHash = "UNSIGNED-PAYLOAD"
		expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
		if err != nil || expires <= 0 || expires > 7*24*3600 {
			return "AuthorizationQueryParametersError", "X-Amz-Expires must be between 1 and 604800"
		}
		t, err := time.Parse("20060102T150405Z", amzDate)
		if err != nil {
			return "AuthorizationQueryParametersError", "bad X-Amz-Date"
		}
		s.mu.Lock()
		now := s.now()
		s.mu.Unlock()
		if now.After(t.Add(time.Duration(expires) * time.Second)) {
			return "AccessDenied", "Request has expired"
		}
	} else {
		auth := r.Header.Get("Authorization")
		rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
		if !ok {
			return "AccessDenied", "missing or unsupported Authorization"
		}
		for _, part := range strings.Split(rest, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch k {
			case "Credential":
				credential = v
			case "SignedHeaders":
				signedHeaders = v
			case "Signature":
				signature = v
			}
		}
		amzDate = r.Header.Get("X-Amz-Date")
		payloadHash = r.Header.Get("X-Amz-Content-Sha256")
		if payloadHash == "" {
			return "InvalidRequest", "Missing required header for this request: x-amz-content-sha256"
		}
		if payloadHash != "UNSIGNED-PAYLOAD" {
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != payloadHash {
				return "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed."
			}
		}
	}

	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" || parts[3] != "s3" {
		return "AuthorizationHeaderMalformed", "bad credential scope " + credential
	}
	if parts[0] != AccessKeyID {
		return "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records."
	}
	if parts[2] != Region {
		return "AuthorizationHeaderMalformed", "the region '" + parts[2] + "' is wrong; expecting '" + Region + "'"
	}
	if len(amzDate) < 8 || amzDate[:8] != parts[1] {
		return "AuthorizationHeaderMalformed", "date mismatch"
	}

	signed := strings.Split(signedHeaders, ";")
	if !slices.Contains(signed, "host") {
		return "AccessDenied", "host must be signed"
	}
	// Every x-amz-* header sent must be signed (but the signature itself
	// and, for header auth, the content hash and date are).
	for k := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") && !slices.Contains(signed, lk) && lk != "x-amz-content-sha256" {
			return "AccessDenied", "There were headers present in the request which were not signed: " + lk
		}
		if lk == "content-md5" && !slices.Contains(signed, lk) {
			return "AccessDenied", "There were headers present in the request which were not signed: content-md5"
		}
	}

	var ch strings.Builder
	for _, h := range signed {
		var v string
		switch h {
		case "host":
			v = r.Host
		case "content-length":
			v = strconv.FormatInt(r.ContentLength, 10)
		default:
			vs := r.Header.Values(h)
			if len(vs) == 0 {
				return "AccessDenied", "signed header " + h + " is missing"
			}
			for i := range vs {
				vs[i] = strings.Join(strings.Fields(vs[i]), " ")
			}
			v = strings.Join(vs, ",")
		}
		ch.WriteString(h + ":" + v + "\n")
	}

	canonical := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		canonicalQuery(q, presigned),
		ch.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := strings.Join(parts[1:], "/")
	crHash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crHash[:])
	key := hmacSHA256([]byte("AWS4"+SecretAccessKey), parts[1])
	key = hmacSHA256(key, parts[2])
	key = hmacSHA256(key, parts[3])
	key = hmacSHA256(key, "aws4_request")
	want := hex.EncodeToString(hmacSHA256(key, toSign))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided."
	}
	return "", ""
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQuery is the SigV4 canonical query string; a presigned
// request's own signature is left out.
func canonicalQuery(q url.Values, presigned bool) string {
	var pairs []string
	for k, vs := range q {
		if presigned && k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k)+"="+uriEncode(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
