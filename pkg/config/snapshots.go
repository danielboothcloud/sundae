package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Snapshot triggers: the values snapshots.s3.on accepts.
const (
	// SnapshotOnEnd snapshots /sandbox before `session end` deletes the
	// sandbox. A failed snapshot stops the end (session end --force ends
	// without it).
	SnapshotOnEnd = "end"
	// SnapshotOnPark snapshots /sandbox before a memory-retaining park
	// (suspend), as a backup. A failed backup does not stop the park.
	SnapshotOnPark = "park"
)

// SnapshotTriggers lists every valid snapshots.s3.on value.
var SnapshotTriggers = []string{SnapshotOnEnd, SnapshotOnPark}

// Server-side encryption modes: the values snapshots.s3.sse accepts.
const (
	// SSENone sends no server-side encryption header; the bucket's
	// default encryption, if any, applies.
	SSENone = ""
	// SSES3 asks for SSE-S3 (AES256, keys managed by the store).
	SSES3 = "AES256"
	// SSEKMS asks for SSE-KMS with the bucket's default KMS key.
	SSEKMS = "aws:kms"
)

// Snapshot defaults.
const (
	// DefaultSnapshotPrefix is the snapshots.s3.prefix default.
	DefaultSnapshotPrefix = "sundae/sessions"
	// DefaultSnapshotRegion is the region signed for an S3-compatible
	// endpoint (snapshots.s3.endpoint set) when snapshots.s3.region is
	// empty. MinIO and Ceph RGW accept it unless configured otherwise.
	DefaultSnapshotRegion = "us-east-1"
)

// DefaultSnapshotOn is the snapshots.s3.on default.
var DefaultSnapshotOn = []string{SnapshotOnEnd}

// SnapshotsConfig configures snapshots of a session's /sandbox.
type SnapshotsConfig struct {
	// S3 stores snapshots in an S3 bucket (AWS or S3-compatible).
	S3 S3SnapshotConfig `envPrefix:"S3_" yaml:"s3"`
}

// S3SnapshotConfig configures S3 snapshots of /sandbox.
//
// Off by default. While Enabled is false nothing runs and nothing in this
// section is validated beyond its YAML types.
type S3SnapshotConfig struct {
	// Enabled turns S3 snapshots on.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// Endpoint is the base URL of an S3-compatible store (MinIO, Ceph
	// RGW, ...), e.g. http://rook-ceph-rgw.rook-ceph.svc:80. Empty means
	// AWS S3 in Region. The sandboxes reach it too: they upload and
	// download the snapshot themselves, through presigned URLs.
	Endpoint string `env:"ENDPOINT" yaml:"endpoint"`

	// Region is the region requests are signed for. Required for AWS;
	// defaults to DefaultSnapshotRegion with an Endpoint.
	Region string `env:"REGION" yaml:"region"`

	// Bucket holds the snapshots. It must exist.
	Bucket string `env:"BUCKET" yaml:"bucket"`

	// Prefix is prepended to every object key:
	// <prefix>/<user-id>/<sandbox>.tar.gz. Defaults to
	// DefaultSnapshotPrefix.
	Prefix string `env:"PREFIX" yaml:"prefix"`

	// CredentialsFile is the path of a file holding the access key, in
	// the AWS shared-credentials format: aws_access_key_id,
	// aws_secret_access_key and optionally aws_session_token, in a
	// [default] section or with no section. KEY=value lines with the
	// environment names (AWS_ACCESS_KEY_ID, ...) are read too. It may
	// also be a directory with one file per key, named
	// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and optionally
	// AWS_SESSION_TOKEN, as a Kubernetes Secret with those keys (a Rook
	// ObjectBucketClaim's) mounts. It is read again for every operation,
	// so rotated keys are picked up without a restart. Required when
	// enabled.
	CredentialsFile string `env:"CREDENTIALS_FILE" yaml:"credentials_file"`

	// SSE is the server-side encryption to ask for: AES256, aws:kms, or
	// "" (the default) to ask for none, so the bucket's default applies:
	// AWS S3 encrypts every new object with SSE-S3 by default, and MinIO
	// and Ceph RGW refuse an SSE header unless they have a KMS configured.
	SSE string `env:"SSE" yaml:"sse"`

	// PathStyle addresses the bucket as <endpoint>/<bucket>/<key> instead
	// of <bucket>.<endpoint>/<key>. Most MinIO and Ceph RGW deployments
	// need it.
	PathStyle bool `env:"PATH_STYLE" yaml:"path_style"`

	// On lists when a snapshot is taken: end (before `session end`
	// deletes the sandbox) and park (before a memory-retaining park, as
	// a backup). Defaults to DefaultSnapshotOn. Restores do not depend
	// on it.
	On []string `env:"ON" yaml:"on"`
}

// Snapshots reports whether a snapshot is taken on trigger.
func (s S3SnapshotConfig) Snapshots(trigger string) bool {
	return s.Enabled && slices.Contains(s.On, trigger)
}

// S3Credentials are the access key of the snapshot store.
type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Credentials reads CredentialsFile. It is read on every call.
func (s S3SnapshotConfig) Credentials() (S3Credentials, error) {
	if s.CredentialsFile == "" {
		return S3Credentials{}, errors.New("snapshots.s3.credentials_file is not set")
	}
	if fi, err := os.Stat(s.CredentialsFile); err == nil && fi.IsDir() {
		creds, err := readS3CredentialsDir(s.CredentialsFile)
		if err != nil {
			return S3Credentials{}, fmt.Errorf("snapshots.s3: credentials_file %q: %w", s.CredentialsFile, err)
		}
		return creds, nil
	}
	bts, err := os.ReadFile(s.CredentialsFile)
	if err != nil {
		return S3Credentials{}, fmt.Errorf("snapshots.s3: read credentials_file: %w", err)
	}
	creds, err := parseS3Credentials(bts)
	if err != nil {
		return S3Credentials{}, fmt.Errorf("snapshots.s3: credentials_file %q: %w", s.CredentialsFile, err)
	}
	return creds, nil
}

// readS3CredentialsDir reads a directory holding one file per key, named
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and optionally
// AWS_SESSION_TOKEN: a Kubernetes Secret with those keys mounted as a
// volume, as a Rook ObjectBucketClaim creates it.
func readS3CredentialsDir(dir string) (S3Credentials, error) {
	read := func(name string, required bool) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist) && !required:
			return "", nil
		case err != nil:
			return "", fmt.Errorf("the directory has no readable %s: %w", name, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" && required {
			return "", fmt.Errorf("%s is empty", name)
		}
		return v, nil
	}
	var c S3Credentials
	var err error
	if c.AccessKeyID, err = read("AWS_ACCESS_KEY_ID", true); err != nil {
		return S3Credentials{}, err
	}
	if c.SecretAccessKey, err = read("AWS_SECRET_ACCESS_KEY", true); err != nil {
		return S3Credentials{}, err
	}
	if c.SessionToken, err = read("AWS_SESSION_TOKEN", false); err != nil {
		return S3Credentials{}, err
	}
	return c, nil
}

// parseS3Credentials parses an AWS shared-credentials file. Only the
// [default] section is read when there are sections; keys before any
// section count too, so a file of AWS_ACCESS_KEY_ID=... lines works.
// Keys are case-insensitive. Values are never quoted in that format; a pair
// of surrounding quotes is removed anyway.
func parseS3Credentials(b []byte) (S3Credentials, error) {
	var c S3Credentials
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "" && section != "default" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(k), "export ")))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "aws_access_key_id":
			c.AccessKeyID = v
		case "aws_secret_access_key":
			c.SecretAccessKey = v
		case "aws_session_token":
			c.SessionToken = v
		}
	}
	if err := sc.Err(); err != nil {
		return S3Credentials{}, err //nolint:wrapcheck
	}
	switch {
	case c.AccessKeyID == "" && c.SecretAccessKey == "":
		return S3Credentials{}, errors.New("no aws_access_key_id and aws_secret_access_key in the [default] section")
	case c.AccessKeyID == "":
		return S3Credentials{}, errors.New("aws_access_key_id is missing")
	case c.SecretAccessKey == "":
		return S3Credentials{}, errors.New("aws_secret_access_key is missing")
	}
	return c, nil
}

// bucketRe matches a bucket name S3 and its look-alikes accept.
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// regionRe matches a region name.
var regionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// validateSnapshots validates the snapshots section. Nothing is checked
// while snapshots.s3.enabled is false. Every problem is reported, each
// naming its key.
func (c *Config) validateSnapshots() error {
	s := &c.Snapshots.S3
	if !s.Enabled {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("snapshots.s3."+format, args...))
	}

	s.Endpoint = strings.TrimSuffix(strings.TrimSpace(s.Endpoint), "/")
	if s.Endpoint != "" {
		if err := validateS3Endpoint(s.Endpoint); err != nil {
			fail("endpoint %q: %v", s.Endpoint, err)
		}
	}

	s.Region = strings.TrimSpace(s.Region)
	switch {
	case s.Region == "" && s.Endpoint == "":
		fail("region is required for AWS S3 (or set snapshots.s3.endpoint for an S3-compatible store)")
	case s.Region == "":
		s.Region = DefaultSnapshotRegion
	case !regionRe.MatchString(s.Region):
		fail("region %q is not a region name", s.Region)
	}

	s.Bucket = strings.TrimSpace(s.Bucket)
	switch {
	case s.Bucket == "":
		fail("bucket is required when snapshots.s3.enabled is true")
	case !bucketRe.MatchString(s.Bucket) || strings.Contains(s.Bucket, ".."):
		fail("bucket %q is not a valid bucket name: 3 to 63 lowercase letters, digits, '.' and '-'", s.Bucket)
	case strings.Contains(s.Bucket, ".") && !s.PathStyle && strings.HasPrefix(s.Endpoint, "https://"):
		fail("bucket %q contains '.', which breaks TLS with virtual-hosted addressing; set snapshots.s3.path_style: true", s.Bucket)
	}

	s.Prefix = strings.Trim(strings.TrimSpace(s.Prefix), "/")
	if s.Prefix == "" {
		s.Prefix = DefaultSnapshotPrefix
	}
	for _, seg := range strings.Split(s.Prefix, "/") {
		if seg == "" || seg == "." || seg == ".." || !repoSegmentRe.MatchString(seg) {
			fail("prefix %q: use path segments of letters, digits, '_', '.' and '-', e.g. %s", s.Prefix, DefaultSnapshotPrefix)
			break
		}
	}

	if s.CredentialsFile == "" {
		fail("credentials_file is required when snapshots.s3.enabled is true")
	} else {
		if !filepath.IsAbs(s.CredentialsFile) {
			s.CredentialsFile = filepath.Join(c.DataPath, s.CredentialsFile)
		}
		// Read once now so a missing or malformed file fails at startup.
		// The keys are not kept: Credentials reads the file again.
		if _, err := s.Credentials(); err != nil {
			errs = append(errs, err)
		}
	}

	s.SSE = strings.TrimSpace(s.SSE)
	switch s.SSE {
	case SSENone, SSES3, SSEKMS:
	default:
		fail("sse %q must be %s, %s or \"\" (none)", s.SSE, SSES3, SSEKMS)
	}

	if len(s.On) == 0 {
		s.On = slices.Clone(DefaultSnapshotOn)
	}
	on := make([]string, 0, len(s.On))
	for i, t := range s.On {
		t = strings.ToLower(strings.TrimSpace(t))
		switch {
		case !slices.Contains(SnapshotTriggers, t):
			fail("on[%d] %q must be %s", i, s.On[i], strings.Join(SnapshotTriggers, " or "))
		case !slices.Contains(on, t):
			on = append(on, t)
		}
	}
	s.On = on

	return errors.Join(errs...)
}

// validateS3Endpoint checks an S3 endpoint: an http or https URL with a
// host and nothing after it.
func validateS3Endpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err //nolint:wrapcheck
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must be an http or https URL")
	case u.Host == "":
		return errors.New("has no host")
	case u.User != nil:
		return errors.New("must not include credentials; use snapshots.s3.credentials_file")
	case u.Path != "" || u.RawQuery != "" || u.Fragment != "":
		return errors.New("must be a base URL, without a path or query")
	}
	return nil
}

// snapshotsEnviron returns the snapshots section as SUNDAE_SNAPSHOTS_*
// environment variables. The keys never are: only the file holding them.
func (c *Config) snapshotsEnviron() []string {
	s := c.Snapshots.S3
	return []string{
		fmt.Sprintf("SUNDAE_SNAPSHOTS_S3_ENABLED=%t", s.Enabled),
		"SUNDAE_SNAPSHOTS_S3_ENDPOINT=" + s.Endpoint,
		"SUNDAE_SNAPSHOTS_S3_REGION=" + s.Region,
		"SUNDAE_SNAPSHOTS_S3_BUCKET=" + s.Bucket,
		"SUNDAE_SNAPSHOTS_S3_PREFIX=" + s.Prefix,
		"SUNDAE_SNAPSHOTS_S3_CREDENTIALS_FILE=" + s.CredentialsFile,
		"SUNDAE_SNAPSHOTS_S3_SSE=" + s.SSE,
		fmt.Sprintf("SUNDAE_SNAPSHOTS_S3_PATH_STYLE=%t", s.PathStyle),
		"SUNDAE_SNAPSHOTS_S3_ON=" + strings.Join(s.On, ","),
	}
}
