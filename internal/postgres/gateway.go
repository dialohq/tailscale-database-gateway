package postgres

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"
	"unicode"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	connectionproxy "github.com/dialohq/tailscale-database-gateway/internal/proxy"
	"github.com/dialohq/tailscale-database-gateway/internal/service"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

type Gateway struct {
	HandshakeTimeout time.Duration
	RetryAttempts    int
	RetryDelay       time.Duration
	Authorize        func(context.Context, string, string) (credentials.Identity, credentials.Provider, error)
	Connector        Connector
	CancelDialer     interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	}
	Logger        *slog.Logger
	cancellations cancellationRegistry
}

func (g *Gateway) Serve(ctx context.Context, listener net.Listener) error {
	return service.ServeConnections(ctx, listener, g.handle)
}

func (g *Gateway) handle(ctx context.Context, client net.Conn) {
	defer client.Close()
	remote := client.RemoteAddr().String()
	clientIP := remote
	if host, _, err := net.SplitHostPort(remote); err == nil {
		clientIP = host
	}
	logger := g.Logger.With("remote", remote)
	if err := client.SetReadDeadline(time.Now().Add(g.HandshakeTimeout)); err != nil {
		logger.Warn("failed to set client startup deadline", "error", err)
		return
	}
	initial, err := readInitialPacket(client)
	if err != nil {
		logger.Warn("failed to read PostgreSQL startup packet", "error", err)
		return
	}
	if initial.Cancel != nil {
		g.handleCancel(ctx, clientIP, initial.Cancel, logger)
		return
	}
	startup := initial.Startup
	role, database := startup.Parameters["user"], startup.Parameters["database"]
	logger = logger.With("client_user", role, "database", database)
	authorizeContext, cancelAuthorize := context.WithTimeout(ctx, g.HandshakeTimeout)
	identity, provider, err := g.Authorize(authorizeContext, remote, role)
	cancelAuthorize()
	if err != nil {
		logger.Warn("failed to authorize Tailscale identity", "error", err)
		writeFatal(client, "28000", "the PostgreSQL gateway denied this connection")
		return
	}
	logger = logger.With("login", identity.LoginName, "node", identity.NodeName)
	credentials, upstream, unrecognized, err := g.connect(ctx, identity, role, database, startup, provider)
	if err != nil {
		logger.Warn("failed to establish upstream PostgreSQL connection", "error", err)
		writeFatal(client, "08006", "the PostgreSQL gateway could not establish a database connection")
		return
	}
	defer credentials.Finish(g.Logger, "PostgreSQL", role, false)
	defer upstream.Conn.Close()
	if !g.cancellations.register(upstream, clientIP) {
		logger.Error("upstream PostgreSQL cancellation key collided with an active connection")
		return
	}
	defer g.cancellations.unregister(upstream, clientIP)
	if err := client.SetDeadline(time.Time{}); err != nil {
		logger.Warn("failed to clear client startup deadline", "error", err)
		return
	}
	if err := writeStartupSuccess(client, upstream, unrecognized); err != nil {
		logger.Debug("client closed during PostgreSQL startup response", "error", err)
		return
	}
	logger.Info("authorized PostgreSQL connection", "role", role, "upstream_user", credentials.Username)
	connectionproxy.Proxy(ctx, client, upstream.Conn)
}

func (g *Gateway) connect(ctx context.Context, identity credentials.Identity, role, database string, startup *pgproto3.StartupMessage, provider credentials.Provider) (credentials.Credentials, *Upstream, []string, error) {
	attempts := max(g.RetryAttempts, 1)
	runtimeParameters, unrecognized := sanitizeRuntimeParameters(startup.Parameters, auditApplicationName(identity))
	if startup.ProtocolVersion == pgproto3.ProtocolVersion30 {
		unrecognized = nil
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		loaded, err := provider(ctx, identity, false)
		if err != nil {
			return credentials.Credentials{}, nil, nil, err
		}
		connectContext, cancelConnect := context.WithTimeout(ctx, g.HandshakeTimeout)
		upstream, err := g.Connector.Connect(connectContext, ConnectRequest{Username: loaded.Username, Password: loaded.Password, Database: database, RuntimeParameters: runtimeParameters})
		cancelConnect()
		if err == nil {
			return loaded, upstream, unrecognized, nil
		}
		var postgresError *pgconn.PgError
		rejected := errors.As(err, &postgresError) && (postgresError.Code == "28P01" || postgresError.Code == "28000")
		loaded.Finish(g.Logger, "PostgreSQL", role, rejected)
		if !rejected || attempt == attempts || !service.Wait(ctx, g.RetryDelay) {
			return credentials.Credentials{}, nil, nil, err
		}
		g.Logger.Info("upstream rejected PostgreSQL credentials; reloading and retrying", "attempt", attempt, "role", role)
	}
	panic("unreachable")
}

func auditApplicationName(identity credentials.Identity) string {
	value := strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return '_'
		}
		return character
	}, "tailscale:"+identity.LoginName+"@"+identity.NodeName)
	if len(value) > 63 {
		return strings.ToValidUTF8(value[:63], "")
	}
	return value
}
