package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
)

const testS3Creds = "[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY\n"

func TestSnapshotsDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	s := cfg.Snapshots.S3
	is.True(!s.Enabled)
	is.Equal(s.Prefix, "soft-serve/sessions")
	is.Equal(s.SSE, "")
	is.Equal(s.On, []string{"end"})
	is.Equal(cfg.Sessions.Suspend.Fallback, "none")
	is.True(!cfg.SnapshotFallback())
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.Validate())
}

func TestValidateSnapshots(t *testing.T) {
	valid := func(t *testing.T) *Config {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Snapshots.S3 = S3SnapshotConfig{
			Enabled:         true,
			Region:          "eu-west-2",
			Bucket:          "agent-snapshots",
			CredentialsFile: writeTokenFile(t, cfg.DataPath, "s3-credentials", testS3Creds),
		}
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		errs   []string
	}{
		"valid aws": {mutate: func(*Config) {}},
		"disabled checks nothing": {mutate: func(c *Config) {
			c.Snapshots.S3 = S3SnapshotConfig{Endpoint: "ftp://x/y", Region: "bad region", Bucket: "Bad_Bucket", SSE: "rot13", On: []string{"never"}, CredentialsFile: "missing"}
		}},
		"rgw endpoint, default region": {mutate: func(c *Config) {
			c.Snapshots.S3.Endpoint = "http://rook-ceph-rgw-store.rook-ceph.svc:80/"
			c.Snapshots.S3.Region = ""
			c.Snapshots.S3.PathStyle = true
			c.Snapshots.S3.SSE = ""
		}},
		"park trigger and fallback": {mutate: func(c *Config) {
			c.Snapshots.S3.On = []string{"End", "park", "end"}
			c.Sessions.Suspend.Fallback = "snapshot"
		}},
		"kms": {mutate: func(c *Config) { c.Snapshots.S3.SSE = "aws:kms" }},
		"aws without region": {
			mutate: func(c *Config) { c.Snapshots.S3.Region = "" },
			errs:   []string{"snapshots.s3.region is required for AWS S3"},
		},
		"bad endpoint": {
			mutate: func(c *Config) { c.Snapshots.S3.Endpoint = "rgw.local:8080" },
			errs:   []string{`snapshots.s3.endpoint "rgw.local:8080"`},
		},
		"endpoint with path": {
			mutate: func(c *Config) { c.Snapshots.S3.Endpoint = "https://s3.example.com/bucket" },
			errs:   []string{`snapshots.s3.endpoint "https://s3.example.com/bucket": must be a base URL`},
		},
		"endpoint with credentials": {
			mutate: func(c *Config) { c.Snapshots.S3.Endpoint = "https://ak:sk@s3.example.com" },
			errs:   []string{"snapshots.s3.endpoint", "must not include credentials"},
		},
		"missing bucket": {
			mutate: func(c *Config) { c.Snapshots.S3.Bucket = "" },
			errs:   []string{"snapshots.s3.bucket is required"},
		},
		"bad bucket": {
			mutate: func(c *Config) { c.Snapshots.S3.Bucket = "My_Bucket" },
			errs:   []string{`snapshots.s3.bucket "My_Bucket" is not a valid bucket name`},
		},
		"dotted bucket over https needs path style": {
			mutate: func(c *Config) {
				c.Snapshots.S3.Endpoint = "https://s3.example.com"
				c.Snapshots.S3.Bucket = "a.b"
			},
			errs: []string{`snapshots.s3.bucket "a.b" contains '.'`},
		},
		"bad prefix": {
			mutate: func(c *Config) { c.Snapshots.S3.Prefix = "a/../b" },
			errs:   []string{`snapshots.s3.prefix "a/../b"`},
		},
		"missing credentials file": {
			mutate: func(c *Config) { c.Snapshots.S3.CredentialsFile = "" },
			errs:   []string{"snapshots.s3.credentials_file is required"},
		},
		"unreadable credentials file": {
			mutate: func(c *Config) { c.Snapshots.S3.CredentialsFile = "nope" },
			errs:   []string{"snapshots.s3: read credentials_file"},
		},
		"credentials without secret": {
			mutate: func(c *Config) {
				c.Snapshots.S3.CredentialsFile = writeTokenFile(t, c.DataPath, "half", "aws_access_key_id=AK\n")
			},
			errs: []string{"snapshots.s3: credentials_file", "aws_secret_access_key is missing"},
		},
		"bad sse": {
			mutate: func(c *Config) { c.Snapshots.S3.SSE = "aes256" },
			errs:   []string{`snapshots.s3.sse "aes256" must be AES256, aws:kms or "" (none)`},
		},
		"bad trigger": {
			mutate: func(c *Config) { c.Snapshots.S3.On = []string{"end", "suspend"} },
			errs:   []string{`snapshots.s3.on[1] "suspend" must be end or park`},
		},
		"fallback without s3": {
			mutate: func(c *Config) {
				c.Snapshots.S3.Enabled = false
				c.Sessions.Suspend.Fallback = "snapshot"
			},
			errs: []string{`sessions.suspend.fallback "snapshot" needs snapshots.s3.enabled: true`},
		},
		"bad fallback": {
			mutate: func(c *Config) { c.Sessions.Suspend.Fallback = "delete" },
			errs:   []string{`sessions.suspend.fallback "delete" must be none or snapshot`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if len(tc.errs) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.errs)
			}
			for _, want := range tc.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateSnapshotsNormalises(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	writeTokenFile(t, cfg.DataPath, "creds", testS3Creds)
	cfg.Snapshots.S3 = S3SnapshotConfig{
		Enabled:         true,
		Endpoint:        " http://minio:9000/ ",
		Bucket:          "snaps",
		Prefix:          "/team/sessions/",
		CredentialsFile: "creds",
		On:              []string{" Park ", "park"},
	}
	cfg.Sessions.Suspend.Fallback = " Snapshot "
	is.NoErr(cfg.Validate())
	s := cfg.Snapshots.S3
	is.Equal(s.Endpoint, "http://minio:9000")
	is.Equal(s.Region, "us-east-1")
	is.Equal(s.Prefix, "team/sessions")
	is.Equal(s.CredentialsFile, filepath.Join(cfg.DataPath, "creds"))
	is.Equal(s.On, []string{"park"})
	is.True(s.Snapshots("park"))
	is.True(!s.Snapshots("end"))
	is.True(cfg.SnapshotFallback())

	// Empty on falls back to the default.
	cfg.Snapshots.S3.On = nil
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Snapshots.S3.On, []string{"end"})
}

func TestS3Credentials(t *testing.T) {
	cases := map[string]struct {
		file string
		want S3Credentials
		err  string
	}{
		"shared credentials": {
			file: "# comment\n[other]\naws_access_key_id = OTHER\n[default]\naws_access_key_id = AK\naws_secret_access_key = SK\naws_session_token = ST\n",
			want: S3Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "ST"},
		},
		"env style": {
			file: "export AWS_ACCESS_KEY_ID=\"AK\"\nAWS_SECRET_ACCESS_KEY='SK'\n",
			want: S3Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"},
		},
		"only another profile": {
			file: "[prod]\naws_access_key_id = AK\naws_secret_access_key = SK\n",
			err:  "no aws_access_key_id and aws_secret_access_key in the [default] section",
		},
		"no key id": {file: "aws_secret_access_key=SK\n", err: "aws_access_key_id is missing"},
	}
	// A mounted Secret with one file per key.
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		for k, v := range map[string]string{"AWS_ACCESS_KEY_ID": "AK\n", "AWS_SECRET_ACCESS_KEY": "SK"} {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := S3SnapshotConfig{CredentialsFile: dir}.Credentials()
		if err != nil || got != (S3Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}) {
			t.Fatalf("got %+v, %v", got, err)
		}
		if err := os.Remove(filepath.Join(dir, "AWS_SECRET_ACCESS_KEY")); err != nil {
			t.Fatal(err)
		}
		if _, err := (S3SnapshotConfig{CredentialsFile: dir}).Credentials(); err == nil || !strings.Contains(err.Error(), "no readable AWS_SECRET_ACCESS_KEY") {
			t.Fatalf("missing secret: %v", err)
		}
	})
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "creds")
			if err := os.WriteFile(p, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := S3SnapshotConfig{CredentialsFile: p}.Credentials()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestSnapshotsEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataPath = td
	cfg.Snapshots.S3 = S3SnapshotConfig{
		Enabled:         true,
		Endpoint:        "http://minio.minio.svc:9000",
		Region:          "us-east-1",
		Bucket:          "snaps",
		Prefix:          "soft/sessions",
		CredentialsFile: writeTokenFile(t, td, "s3", testS3Creds),
		SSE:             "",
		PathStyle:       true,
		On:              []string{"end", "park"},
	}
	cfg.Sessions.Suspend.Fallback = "snapshot"
	is.NoErr(cfg.Validate())

	environ := cfg.Environ()
	is.True(slices.Contains(environ, "SOFT_SERVE_SNAPSHOTS_S3_ON=end,park"))
	is.True(slices.Contains(environ, "SOFT_SERVE_SESSIONS_SUSPEND_FALLBACK=snapshot"))
	for _, e := range environ {
		is.True(!strings.Contains(e, "wJalrXUtnFEMI")) // no secret in the environment
		is.True(!strings.Contains(e, "AKIDEXAMPLE"))
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	got := DefaultConfig()
	is.NoErr(got.ParseEnv())
	is.Equal(got.Snapshots, cfg.Snapshots)
	is.Equal(got.Sessions.Suspend.Fallback, "snapshot")
}

func TestSnapshotsConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Snapshots.S3 = S3SnapshotConfig{
		Enabled:         true,
		Region:          "eu-west-2",
		Bucket:          "snaps",
		Prefix:          "x/y",
		CredentialsFile: writeTokenFile(t, cfg.DataPath, "s3", testS3Creds),
		SSE:             "aws:kms",
		On:              []string{"park"},
	}
	cfg.Sessions.Suspend.Fallback = "snapshot"
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Snapshots, cfg.Snapshots)
	is.Equal(got.Sessions.Suspend.Fallback, "snapshot")

	// The default template parses, keeps snapshots off and checks nothing.
	def := DefaultConfig()
	def.DataPath = t.TempDir()
	is.NoErr(def.WriteConfig())
	got = &Config{DataPath: def.DataPath}
	is.NoErr(parseFile(got, def.ConfigPath()))
	is.True(!got.Snapshots.S3.Enabled)
	is.Equal(got.Snapshots.S3.On, []string{"end"})
}

func TestSnapshotsUnknownKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`snapshots:
  s3:
    enabled: false
    access_key: AK
    true: [end]
`), 0o600))
	err := DefaultConfig().Load(p)
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "line 4: unknown key snapshots.s3.access_key"))
	is.True(strings.Contains(err.Error(), "line 5: unknown key snapshots.s3.true (an unquoted"))
}
