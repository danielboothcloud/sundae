package backend

import (
	"context"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/store"
	"github.com/danielboothcloud/sundae/pkg/task"
)

// Backend is the Sundae backend that handles users, repositories, and
// server settings management and operations.
type Backend struct {
	ctx     context.Context
	cfg     *config.Config
	db      *db.DB
	store   store.Store
	logger  *log.Logger
	manager *task.Manager
}

// New returns a new Sundae backend.
func New(ctx context.Context, cfg *config.Config, db *db.DB, st store.Store) *Backend {
	logger := log.FromContext(ctx).WithPrefix("backend")
	b := &Backend{
		ctx:     ctx,
		cfg:     cfg,
		db:      db,
		store:   st,
		logger:  logger,
		manager: task.NewManager(ctx),
	}

	return b
}
