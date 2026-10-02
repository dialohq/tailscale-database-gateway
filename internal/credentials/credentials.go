package credentials

import (
	"context"
	"log/slog"
)

type Identity struct {
	LoginName string
	NodeName  string
	NodeID    string
}

type Credentials struct {
	Username string           `json:"username"`
	Password string           `json:"password"`
	Complete func(bool) error `json:"-"`
}

func (c Credentials) Finish(logger *slog.Logger, database, role string, rejected bool) {
	if c.Complete == nil {
		return
	}
	if err := c.Complete(rejected); err != nil {
		logger.Warn("failed to finish database credentials", "database", database, "role", role, "rejected", rejected, "error", err)
	}
}

type Provider func(context.Context, Identity, bool) (Credentials, error)

type Roles map[string]Provider

func NewRoles[T any](configured map[string]T, load func(context.Context, Identity, string, T, bool) (Credentials, error)) Roles {
	roles := make(Roles, len(configured))
	for role, source := range configured {
		roles[role] = func(ctx context.Context, identity Identity, reusable bool) (Credentials, error) {
			return load(ctx, identity, role, source, reusable)
		}
	}
	return roles
}
