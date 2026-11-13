package repo

import (
	"fmt"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

func defaultEmptyRepoMsg(cfg *config.Config, repo string, vcs proto.VCS) string {
	if vcs == proto.VCSJJ {
		return jjEmptyRepoMsg(common.RepoURL(cfg.SSH.PublicURL, repo))
	}
	return fmt.Sprintf(`# Quick Start

Get started by cloning this repository, add your files, commit, and push.

## Clone this repository.

`+"```"+`sh
git clone %[1]s
`+"```"+`

## Creating a new repository on the command line

`+"```"+`sh
touch README.md
git init
git add README.md
git branch -M main
git commit -m "first commit"
git remote add origin %[1]s
git push -u origin main
`+"```"+`

## Pushing an existing repository from the command line

`+"```"+`sh
git remote add origin %[1]s
git push -u origin main
`+"```"+`
`, common.RepoURL(cfg.SSH.PublicURL, repo))
}

func jjEmptyRepoMsg(url string) string {
	return fmt.Sprintf(`# Quick Start

Get started by cloning this repository, add your files, describe, and push.

## Clone this repository.

`+"```"+`sh
jj git clone %[1]s
`+"```"+`

## Creating a new repository on the command line

`+"```"+`sh
jj git init
echo "# readme" > README.md
jj describe -m "first commit"
jj bookmark create main -r @
jj git remote add origin %[1]s
jj git push --bookmark main
`+"```"+`

## Pushing an existing repository from the command line

`+"```"+`sh
jj git remote add origin %[1]s
jj git push --bookmark main
`+"```"+`
`, url)
}
