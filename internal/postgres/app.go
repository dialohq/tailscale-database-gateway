package postgres

import (
	"context"
	"log/slog"
	"net"
	"time"

	gatewaytailnet "github.com/dialohq/tailscale-database-gateway/internal/tailnet"
)

func (destination destination) ListenPort() uint16 { return destination.port }

func (destination destination) Serve(ctx context.Context, listener net.Listener, network *gatewaytailnet.Network, logger *slog.Logger) error {
	logger = logger.With("destination", destination.name)
	logger.Info("PostgreSQL destination ready", "port", destination.port)
	return (&Gateway{
		HandshakeTimeout: network.Timeout,
		RetryAttempts:    3,
		RetryDelay:       500 * time.Millisecond,
		Authorize:        (&gatewaytailnet.Authorizer{Client: network.Client, Service: network.Service, Capability: destination.capability, Roles: destination.roles}).Authorize,
		Connector:        destination.connector, CancelDialer: &net.Dialer{Timeout: network.Timeout},
		Logger: logger,
	}).Serve(ctx, listener)
}
