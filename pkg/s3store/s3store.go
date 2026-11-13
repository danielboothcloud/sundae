// Package s3store is the small part of the S3 API Sundae's session
// snapshots need: presigned PUT and GET URLs for the sandboxes, which move
// the archives themselves, and a few signed requests Sundae makes with
// small or no bodies (HEAD, GET and PUT of small objects, DELETE, LIST).
//
// Requests are signed with AWS Signature Version 4 by the signer of
// github.com/aws/aws-sdk-go-v2 (its core module only, not the S3 client),
// and addressed to AWS S3 or any S3-compatible endpoint (MinIO, Ceph RGW),
// path-style or virtual-hosted.
package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/danielboothcloud/sundae/pkg/config"
)

// ErrNotFound is returned for an object that does not exist.
var ErrNotFound = errors.New("s3: no such object")

// MaxPutSize is the largest object one PUT may upload: 5 GiB on AWS S3,
// and Ceph RGW's default rgw_max_put_size.
const MaxPutSize int64 = 5 << 30

// emptySHA256 is the SHA-256 of an empty payload.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// unsignedPayload is the payload hash of a presigned URL: the body is not
// part of the signature.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 4 << 10

// Store is one bucket of an S3 store.
type Store struct {
	base      *url.URL
	bucket    string
	region    string
	pathStyle bool
	sse       string
	creds     func() (config.S3Credentials, error)
	signer    *v4.Signer
	client    *http.Client
	now       func() time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithHTTPClient sets the HTTP client of Sundae's own requests.
func WithHTTPClient(c *http.Client) Option { return func(s *Store) { s.client = c } }

// WithClock sets the signing clock.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// New returns the Store snapshots.s3 configures. cfg is expected to be
// validated; the credentials file is read for every request.
func New(cfg config.S3SnapshotConfig, opts ...Option) (*Store, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.Region == "" {
			return nil, errors.New("s3: a region is required for AWS S3")
		}
		endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("s3: invalid endpoint %q", endpoint)
	}
	region := cfg.Region
	if region == "" {
		region = config.DefaultSnapshotRegion
	}
	s := &Store{
		base:      base,
		bucket:    cfg.Bucket,
		region:    region,
		pathStyle: cfg.PathStyle,
		sse:       cfg.SSE,
		creds:     cfg.Credentials,
		signer: v4.NewSigner(func(o *v4.SignerOptions) {
			// S3 signs the path as sent, without escaping it again.
			o.DisableURIPathEscaping = true
			// Presigned uploads send their headers as headers.
			o.DisableHeaderHoisting = true
		}),
		client: &http.Client{Timeout: time.Minute},
		now:    time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Bucket returns the bucket name.
func (s *Store) Bucket() string { return s.bucket }

// BucketURL returns the URL of the bucket, as objects are addressed under
// it: the endpoint, with the bucket as the first label of the host for
// virtual-hosted addressing or as the first path segment for path-style.
// Its scheme and host are where presigned URLs point.
func (s *Store) BucketURL() *url.URL { return s.objectURL("") }

// SSEHeaders returns the server-side encryption headers every upload
// carries; empty when none is configured.
func (s *Store) SSEHeaders() http.Header {
	h := http.Header{}
	if s.sse != "" {
		h.Set("X-Amz-Server-Side-Encryption", s.sse)
	}
	return h
}

// objectURL returns the URL of key, or of the bucket for key "".
func (s *Store) objectURL(key string) *url.URL {
	u := *s.base
	path := "/" + escapeKey(key)
	if s.pathStyle {
		path = "/" + s.bucket + path
	} else {
		u.Host = s.bucket + "." + u.Host
	}
	u.Path, u.RawPath = unescapeOK(path), path
	return &u
}

// escapeKey percent-encodes an object key as S3 expects in a path: every
// byte but the RFC 3986 unreserved characters and '/'.
func escapeKey(key string) string {
	var b strings.Builder
	for _, c := range []byte(key) {
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

// unescapeOK returns the unescaped form of an escaped path.
func unescapeOK(escaped string) string {
	p, err := url.PathUnescape(escaped)
	if err != nil {
		return escaped
	}
	return p
}

func (s *Store) credentials() (aws.Credentials, error) {
	c, err := s.creds()
	if err != nil {
		return aws.Credentials{}, err
	}
	return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken}, nil
}

// Presigned is a presigned request: its URL, and the headers the request
// must send exactly as given, since they are signed.
type Presigned struct {
	URL    string
	Header http.Header
}

// HeaderLines returns the signed headers other than Host as "Name: value"
// lines, sorted, for curl -H.
func (p Presigned) HeaderLines() []string {
	var out []string
	for k, vs := range p.Header {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			out = append(out, k+": "+v)
		}
	}
	slices.Sort(out)
	return out
}

// PutOptions configures a presigned upload.
type PutOptions struct {
	// ContentMD5 is the base64 MD5 of the body. When set it is signed, and
	// S3 rejects a body that does not match.
	ContentMD5 string
	// Metadata is stored with the object as x-amz-meta-<name> headers.
	// Names are lower-cased; values must be ASCII.
	Metadata map[string]string
	// ContentType is the object's media type; empty for none.
	ContentType string
}

// PresignPut presigns a PUT of key, valid for ttl. The upload replaces the
// object only once S3 has stored all of it: until then the previous
// object, if any, stays as it was.
func (s *Store) PresignPut(ctx context.Context, key string, ttl time.Duration, o PutOptions) (Presigned, error) {
	h := s.SSEHeaders()
	if o.ContentMD5 != "" {
		h.Set("Content-Md5", o.ContentMD5)
	}
	if o.ContentType != "" {
		h.Set("Content-Type", o.ContentType)
	}
	for k, v := range o.Metadata {
		h.Set("X-Amz-Meta-"+strings.ToLower(k), v)
	}
	return s.presign(ctx, http.MethodPut, key, ttl, h)
}

// PresignGet presigns a GET of key, valid for ttl.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (Presigned, error) {
	return s.presign(ctx, http.MethodGet, key, ttl, http.Header{})
}

func (s *Store) presign(ctx context.Context, method, key string, ttl time.Duration, h http.Header) (Presigned, error) {
	creds, err := s.credentials()
	if err != nil {
		return Presigned{}, err
	}
	u := s.objectURL(key)
	q := u.Query()
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl/time.Second), 10))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: %w", err)
	}
	req.Header = h
	signed, headers, err := s.signer.PresignHTTP(ctx, creds, req, unsignedPayload, "s3", s.region, s.now())
	if err != nil {
		return Presigned{}, fmt.Errorf("s3: presign %s %s: %w", method, key, err)
	}
	return Presigned{URL: signed, Header: headers}, nil
}

// Object describes a stored object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	// Metadata holds the x-amz-meta-* headers, by lower-case name without
	// the prefix. Only Head fills it in.
	Metadata map[string]string
	// SSE is the server-side encryption the store reports. Only Head
	// fills it in.
	SSE string
}

// do signs and sends a request with an optional small body.
func (s *Store) do(ctx context.Context, method string, u *url.URL, body []byte, h http.Header) (*http.Response, error) {
	creds, err := s.credentials()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	hash := emptySHA256
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if err := s.signer.SignHTTP(ctx, creds, req, hash, "s3", s.region, s.now()); err != nil {
		return nil, fmt.Errorf("s3: sign %s: %w", method, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: %s %s: %w", method, redactURL(u), err)
	}
	return resp, nil
}

// Head returns the object's size, time and metadata; ErrNotFound when it
// does not exist.
func (s *Store) Head(ctx context.Context, key string) (*Object, error) {
	resp, err := s.do(ctx, http.MethodHead, s.objectURL(key), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, responseError("HEAD", key, resp)
	}
	o := &Object{Key: key, Size: resp.ContentLength, Metadata: map[string]string{}, SSE: resp.Header.Get("X-Amz-Server-Side-Encryption")}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		o.LastModified = t
	}
	for k, vs := range resp.Header {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && len(vs) > 0 {
			o.Metadata[name] = vs[0]
		}
	}
	return o, nil
}

// Get returns the body of a small object, at most max bytes; ErrNotFound
// when it does not exist.
func (s *Store) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(key), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, responseError("GET", key, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("s3: GET %s: %w", key, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("s3: GET %s: larger than %d bytes", key, max)
	}
	return b, nil
}

// Put stores a small object, with the configured server-side encryption.
func (s *Store) Put(ctx context.Context, key string, body []byte, contentType string) error {
	h := s.SSEHeaders()
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(key), body, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		return responseError("PUT", key, resp)
	}
	return nil
}

// Delete deletes an object. A missing object is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(key), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return responseError("DELETE", key, resp)
	}
	return nil
}

// listResult is a ListObjectsV2 response.
type listResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List returns the objects whose keys start with prefix, in key order.
func (s *Store) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		u := s.objectURL("")
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		u.RawQuery = q.Encode()
		resp, err := s.do(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			err := responseError("LIST", prefix, resp)
			resp.Body.Close() //nolint:errcheck,gosec
			return nil, err
		}
		var lr listResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&lr)
		resp.Body.Close() //nolint:errcheck,gosec
		if err != nil {
			return nil, fmt.Errorf("s3: LIST %s: %w", prefix, err)
		}
		for _, c := range lr.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, LastModified: c.LastModified})
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			return out, nil
		}
		token = lr.NextContinuationToken
	}
}

// s3Error is an S3 error response body.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// responseError describes a failed response, with S3's error code and
// message when the body has them.
func responseError(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e s3Error
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return fmt.Errorf("s3: %s %s: HTTP %d %s: %s", op, key, resp.StatusCode, e.Code, e.Message)
	}
	return fmt.Errorf("s3: %s %s: HTTP %d", op, key, resp.StatusCode)
}

// redactURL drops the query, which may carry a signature.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	return c.String()
}
