package forges

import (
	"context"
	"io"
	"testing"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

type testUser struct{ admin bool }

func (u testUser) ID() int64                     { return 1 }
func (u testUser) Username() string              { return "u" }
func (u testUser) IsAdmin() bool                 { return u.admin }
func (u testUser) PublicKeys() []gossh.PublicKey { return nil }
func (u testUser) Password() string              { return "" }

// TestNewSourceIsForAdminsOnly checks the tab shows only when forges are
// configured and the user is a server admin, as the `forge` command's
// listing, import and sync are.
func TestNewSourceIsForAdminsOnly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Forges = []config.ForgeConfig{{Name: "gh", Kind: "github", URL: "http://127.0.0.1:1"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc, err := forge.NewService(cfg, nil, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	base := config.WithContext(context.Background(), cfg)
	admin := proto.WithUserContext(base, testUser{admin: true})
	if NewSource(admin) != nil {
		t.Error("a source without forges")
	}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"admin":     {proto.WithUserContext(base, testUser{admin: true}), true},
		"non-admin": {proto.WithUserContext(base, testUser{}), false},
		"anonymous": {base, false},
	} {
		got := NewSource(forge.WithContext(tc.ctx, svc)) != nil
		if got != tc.want {
			t.Errorf("%s: source %v, want %v", name, got, tc.want)
		}
	}
	// A source under SourceKey wins.
	fake := newFakeSource()
	if SourceFor(context.WithValue(base, SourceKey, Source(fake))) != fake {
		t.Error("SourceFor ignored SourceKey")
	}
}
