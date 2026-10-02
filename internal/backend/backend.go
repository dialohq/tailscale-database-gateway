package backend

import (
	"context"
	"log/slog"
	"net"

	"github.com/dialohq/tailscale-database-gateway/internal/tailnet"
)

type Backend interface {
	ListenPort() uint16
	Serve(context.Context, net.Listener, *tailnet.Network, *slog.Logger) error
}
