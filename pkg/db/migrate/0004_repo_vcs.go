package migrate

import (
	"context"

	"github.com/danielboothcloud/sundae/pkg/db"
)

const (
	repoVcsName    = "repo_vcs"
	repoVcsVersion = 4
)

// repoVcs adds the vcs column to repos. Existing repositories are git
// repositories.
var repoVcs = Migration{
	Name:    repoVcsName,
	Version: repoVcsVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, repoVcsVersion, repoVcsName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, repoVcsVersion, repoVcsName)
	},
}
