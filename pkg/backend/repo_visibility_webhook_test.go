package backend

import (
	"context"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/store"
	"github.com/danielboothcloud/sundae/pkg/webhook"
	"github.com/matryer/is"
)

// TestSetPrivateWebhookOnlyOnChange verifies that the visibility change
// webhook fires when visibility changes and not when it is set to the
// value it already has.
func TestSetPrivateWebhookOnlyOnChange(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)

	ctx := context.Background()
	ctx = config.WithContext(ctx, cfg)
	ctx = db.WithContext(ctx, be.db)
	ctx = store.WithContext(ctx, be.store)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	ctx = proto.WithUserContext(ctx, admin)

	repo, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{})
	is.NoErr(err)

	// Deliveries to this address fail fast, but are still recorded.
	var hookID int64
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		hookID, err = be.store.CreateWebhook(ctx, tx, repo.ID(), "http://127.0.0.1:1/", "", int(webhook.ContentTypeJSON), true)
		if err != nil {
			return err
		}
		return be.store.CreateWebhookEvents(ctx, tx, hookID, []int{int(webhook.EventRepositoryVisibilityChange)})
	}))

	deliveries := func() int {
		ds, err := be.store.ListWebhookDeliveriesByWebhookID(ctx, be.db, hookID)
		is.NoErr(err)
		return len(ds)
	}

	is.NoErr(be.SetPrivate(ctx, "repo", false))
	is.Equal(deliveries(), 0)

	is.NoErr(be.SetPrivate(ctx, "repo", true))
	is.Equal(deliveries(), 1)

	is.NoErr(be.SetPrivate(ctx, "repo", true))
	is.Equal(deliveries(), 1)

	is.NoErr(be.SetPrivate(ctx, "repo", false))
	is.Equal(deliveries(), 2)
}
