package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	connectionproxy "github.com/dialohq/tailscale-database-gateway/internal/proxy"
	"github.com/dialohq/tailscale-database-gateway/internal/service"
)

type Gateway struct {
	config
	Authorize func(context.Context, string, string) (Identity, CredentialsProvider, error)
	Dial      func(context.Context, string, string) (net.Conn, error)
	Logger    *slog.Logger
}

func (g *Gateway) Serve(ctx context.Context, listener net.Listener) error {
	return service.ServeConnections(ctx, listener, g.handle)
}

func (g *Gateway) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	remote := client.RemoteAddr().String()
	logger := g.Logger.With("remote", remote)
	handshakeDeadline := time.Now().Add(g.handshakeTimeout)
	handshakeContext, cancelHandshake := context.WithDeadline(ctx, handshakeDeadline)
	defer cancelHandshake()
	if err := client.SetDeadline(handshakeDeadline); err != nil {
		logger.Warn("failed to set client handshake deadline", "error", err)
		return
	}

	hello, err := readClientHello(client)
	if err != nil {
		logger.Warn("failed to read ClickHouse client hello", "error", err)
		return
	}
	logger = logger.With("client_user", hello.username, "database", hello.database)
	identity, provider, err := g.Authorize(handshakeContext, remote, hello.username)
	if err != nil {
		logger.Warn("failed to authorize Tailscale identity", "error", err)
		return
	}
	logger = logger.With("login", identity.LoginName, "node", identity.NodeName)

	attempts := max(g.retryAttempts, 1)
	for attempt := 1; ; attempt++ {
		upstream, packet, credentials, err := g.tryHandshake(handshakeContext, handshakeDeadline, hello, identity, provider)
		if err != nil {
			logger.Warn("upstream ClickHouse handshake failed", "attempt", attempt, "error", err)
			if attempt == attempts || !service.Wait(handshakeContext, g.retryDelay) {
				return
			}
			continue
		}

		if packet == serverExceptionPacket && attempt < attempts {
			logger.Info("upstream rejected credentials; reloading and retrying", "attempt", attempt, "role", hello.username)
			_ = upstream.Close()
			credentials.Finish(g.Logger, "ClickHouse native", hello.username, true)
			if !service.Wait(handshakeContext, g.retryDelay) {
				return
			}
			continue
		}

		defer credentials.Finish(g.Logger, "ClickHouse native", hello.username, packet == serverExceptionPacket)
		defer upstream.Close()
		sessionDeadline, _ := ctx.Deadline()
		if err := client.SetDeadline(sessionDeadline); err != nil {
			logger.Warn("failed to clear client handshake deadline", "error", err)
			return
		}
		if err := upstream.SetDeadline(sessionDeadline); err != nil {
			logger.Warn("failed to clear upstream handshake deadline", "error", err)
			return
		}
		if err := writeUvarint(client, packet); err != nil {
			logger.Debug("client closed during server handshake", "error", err)
			return
		}

		if packet == serverHelloPacket {
			logger.Info("authorized ClickHouse connection", "role", hello.username, "upstream_user", credentials.Username)
		} else {
			logger.Warn("upstream returned a non-hello handshake packet", "packet", packet)
		}
		connectionproxy.Proxy(ctx, client, upstream)
		return
	}
}

func (g *Gateway) tryHandshake(
	ctx context.Context,
	deadline time.Time,
	hello clientHello,
	identity Identity,
	provider CredentialsProvider,
) (_ net.Conn, _ uint64, _ Credentials, resultErr error) {
	credentials, err := provider(ctx, identity, false)
	if err != nil {
		return nil, 0, Credentials{}, err
	}
	var upstream net.Conn
	defer func() {
		if resultErr != nil {
			if upstream != nil {
				_ = upstream.Close()
			}
			credentials.Finish(g.Logger, "ClickHouse native", hello.username, false)
		}
	}()

	encoded, err := hello.encode(credentials.Username, credentials.Password)
	if err != nil {
		return nil, 0, Credentials{}, err
	}

	upstream, err = g.Dial(ctx, "tcp", g.upstream)
	if err != nil {
		return nil, 0, Credentials{}, fmt.Errorf("dial upstream: %w", err)
	}
	if err := upstream.SetDeadline(deadline); err != nil {
		return nil, 0, Credentials{}, fmt.Errorf("set upstream handshake deadline: %w", err)
	}
	if _, err := upstream.Write(encoded); err != nil {
		return nil, 0, Credentials{}, fmt.Errorf("write client hello: %w", err)
	}
	packet, err := readUvarint(upstream)
	if err != nil {
		return nil, 0, Credentials{}, fmt.Errorf("read server packet type: %w", err)
	}
	return upstream, packet, credentials, nil
}
