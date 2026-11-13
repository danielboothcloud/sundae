package common_test

import (
	"testing"

	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

func TestIsFileMarkdown(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		content  string // XXX: chroma doesn't correctly analyze mk files
		isMkd    bool
	}{
		{"simple", "README.md", "", true},
		{"empty", "", "", false},
		{"no extension", "README", "", false},
		{"weird extension", "README.foo", "", false},
		{"long ext", "README.markdown", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := common.IsFileMarkdown(c.content, c.filename); got != c.isMkd {
				t.Errorf("IsFileMarkdown(%q, %q) = %v, want %v", c.content, c.filename, got, c.isMkd)
			}
		})
	}
}

func TestCloneCmd(t *testing.T) {
	c := &common.Common{}
	if got, want := c.CloneCmd("ssh://h", "r", proto.VCSJJ), "jj git clone git@h:r.git"; got != want {
		t.Errorf("jj: got %q, want %q", got, want)
	}
	if got, want := c.CloneCmd("ssh://h", "r", proto.VCSGit), "git clone git@h:r.git"; got != want {
		t.Errorf("git: got %q, want %q", got, want)
	}
	c.HideCloneCmd = true
	if got := c.CloneCmd("ssh://h", "r", proto.VCSJJ); got != "" {
		t.Errorf("hidden: got %q", got)
	}
}
