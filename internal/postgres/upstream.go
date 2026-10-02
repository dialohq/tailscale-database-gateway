package postgres

import (
	"context"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

type ConnectRequest struct {
	Username          string
	Password          string
	Database          string
	RuntimeParameters map[string]string
}

type Upstream struct {
	Conn              net.Conn
	PID               uint32
	SecretKey         []byte
	ParameterStatuses map[string]string
	TxStatus          byte
}

type Connector interface {
	Connect(context.Context, ConnectRequest) (*Upstream, error)
}

type PGXConnector struct {
	config *pgconn.Config
}

func NewPGXConnector(connectionString string) (*PGXConnector, error) {
	config, err := pgconn.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL upstream: %w", err)
	}
	return &PGXConnector{config: config}, nil
}

func (c *PGXConnector) Connect(ctx context.Context, request ConnectRequest) (*Upstream, error) {
	config := c.config.Copy()
	config.User = request.Username
	config.Password = request.Password
	config.Database = request.Database
	config.RuntimeParams = request.RuntimeParameters

	connection, err := pgconn.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := connection.SyncConn(ctx); err != nil {
		_ = connection.Close(ctx)
		return nil, fmt.Errorf("synchronize PostgreSQL connection: %w", err)
	}
	hijacked, err := connection.Hijack()
	if err != nil {
		_ = connection.Close(ctx)
		return nil, fmt.Errorf("hijack PostgreSQL connection: %w", err)
	}
	return &Upstream{
		Conn:              hijacked.Conn,
		PID:               hijacked.PID,
		SecretKey:         hijacked.SecretKey,
		ParameterStatuses: hijacked.ParameterStatuses,
		TxStatus:          hijacked.TxStatus,
	}, nil
}
