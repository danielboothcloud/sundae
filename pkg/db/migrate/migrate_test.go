package migrate

import (
	"context"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/db/internal/test"
)

func TestMigrate(t *testing.T) {
	// XXX: we need a config.Config in the context for the migrations to run
	// properly. Some migrations depend on the config being present.
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Errorf("Migrate() => %v, want nil error", err)
	}
}

// TestMigrateRepoVcs verifies that repositories that exist before the
// repo_vcs migration report the git VCS afterwards.
func TestMigrateRepoVcs(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Run every migration before repo_vcs, then add a legacy repository.
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx, Migrations{}.schema(tx.DriverName())); err != nil {
			return err
		}
		for _, m := range migrations {
			if m.Version >= repoVcsVersion {
				break
			}
			if err := m.Migrate(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, tx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), m.Name, m.Version); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO repos (name, project_name, description, private, mirror, hidden, updated_at, user_id)
			VALUES ('legacy', '', '', false, false, false, CURRENT_TIMESTAMP, 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	var vcs string
	if err := dbx.Get(&vcs, "SELECT vcs FROM repos WHERE name = 'legacy'"); err != nil {
		t.Fatal(err)
	}
	if vcs != "git" {
		t.Errorf("legacy repo vcs = %q, want git", vcs)
	}

	// Rolling back drops the column again.
	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v", err)
	}
	if err := dbx.Get(&vcs, "SELECT vcs FROM repos WHERE name = 'legacy'"); err == nil {
		t.Error("vcs column still present after rollback")
	}
}
