package common

import (
	"context"
	"fmt"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/keymap"
	"github.com/danielboothcloud/sundae/pkg/ui/styles"
	zone "github.com/lrstanley/bubblezone/v2"
)

type contextKey struct {
	name string
}

// Keys to use for context.Context.
var (
	ConfigKey = &contextKey{"config"}
	RepoKey   = &contextKey{"repo"}
)

// Common is a struct all components should embed.
type Common struct {
	ctx           context.Context
	Width, Height int
	Styles        *styles.Styles
	KeyMap        *keymap.KeyMap
	Zone          *zone.Manager
	Logger        *log.Logger
	HideCloneCmd  bool
}

// NewCommon returns a new Common struct.
func NewCommon(ctx context.Context, width, height int) Common {
	if ctx == nil {
		ctx = context.TODO()
	}
	return Common{
		ctx:    ctx,
		Width:  width,
		Height: height,
		Styles: styles.DefaultStyles(),
		KeyMap: keymap.DefaultKeyMap(),
		Zone:   zone.New(),
		Logger: log.FromContext(ctx).WithPrefix("ui"),
	}
}

// SetValue sets a value in the context.
func (c *Common) SetValue(key, value interface{}) {
	c.ctx = context.WithValue(c.ctx, key, value)
}

// SetSize sets the width and height of the common struct.
func (c *Common) SetSize(width, height int) {
	c.Width = width
	c.Height = height
}

// Context returns the context.
func (c *Common) Context() context.Context {
	return c.ctx
}

// Config returns the server config.
func (c *Common) Config() *config.Config {
	return config.FromContext(c.ctx)
}

// Backend returns the Sundae backend.
func (c *Common) Backend() *backend.Backend {
	return backend.FromContext(c.ctx)
}

// Repo returns the repository.
func (c *Common) Repo() *git.Repository {
	v := c.ctx.Value(RepoKey)
	if r, ok := v.(*git.Repository); ok {
		return r
	}
	return nil
}

// PublicKey returns the public key.
func (c *Common) PublicKey() ssh.PublicKey {
	v := c.ctx.Value(ssh.ContextKeyPublicKey)
	if p, ok := v.(ssh.PublicKey); ok {
		return p
	}
	return nil
}

// CloneCmd returns the clone command string.
// jj repositories get "jj git clone", everything else "git clone".
func (c *Common) CloneCmd(publicURL, name string, vcs proto.VCS) string {
	if c.HideCloneCmd {
		return ""
	}
	if vcs == proto.VCSJJ {
		return fmt.Sprintf("jj git clone %s", RepoURL(publicURL, name))
	}
	return fmt.Sprintf("git clone %s", RepoURL(publicURL, name))
}

// IsFileMarkdown returns true if the file is markdown.
// It uses chroma lexers to analyze and determine the language.
func IsFileMarkdown(content, ext string) bool {
	var lang string
	lexer := lexers.Match(ext)
	if lexer == nil {
		lexer = lexers.Analyse(content)
	}
	if lexer != nil && lexer.Config() != nil {
		lang = lexer.Config().Name
	}
	return lang == "markdown"
}

// ScrollPercent returns a string representing the scroll percentage of the
// viewport.
func ScrollPercent(position int) string {
	return fmt.Sprintf("≡ %d%%", position)
}
