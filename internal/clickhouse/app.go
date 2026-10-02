package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/backend"
	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	gatewaytailnet "github.com/dialohq/tailscale-database-gateway/internal/tailnet"
	"tailscale.com/tailcfg"
)

type config struct {
	name               string
	upstream           string
	httpUpstream       *url.URL
	credentialRoles    credentials.Roles
	handshakeTimeout   time.Duration
	retryAttempts      int
	retryDelay         time.Duration
	httpRetryBodyBytes int64
	capability         tailcfg.PeerCapability
}

func New(name string, destination gatewayconfig.Destination) ([]backend.Backend, error) {
	native := destination.NativePort != 0 || destination.NativeUpstream != ""
	http := destination.HTTPPort != 0 || destination.HTTPUpstream != ""
	if destination.Port != 0 || destination.Upstream != "" || !native && !http {
		return nil, fmt.Errorf("ClickHouse destination %q has fields for another backend or no protocol", name)
	}
	if native && http && destination.NativePort == destination.HTTPPort {
		return nil, fmt.Errorf("ClickHouse destination %q uses one port for both protocols", name)
	}
	if native {
		if destination.NativePort == 0 || destination.NativeUpstream == "" {
			return nil, fmt.Errorf("ClickHouse destination %q has an incomplete native configuration", name)
		}
		if _, _, err := net.SplitHostPort(destination.NativeUpstream); err != nil {
			return nil, fmt.Errorf("ClickHouse destination %q has an invalid native configuration", name)
		}
	}
	var httpUpstream *url.URL
	if http {
		if destination.HTTPPort == 0 || destination.HTTPUpstream == "" {
			return nil, fmt.Errorf("ClickHouse destination %q has an incomplete HTTP configuration", name)
		}
		var err error
		httpUpstream, err = url.Parse(destination.HTTPUpstream)
		if err != nil || httpUpstream.Scheme != "http" && httpUpstream.Scheme != "https" || httpUpstream.Host == "" || httpUpstream.User != nil {
			return nil, fmt.Errorf("ClickHouse destination %q has an invalid HTTP configuration", name)
		}
	}
	roles, err := gatewayconfig.CredentialRoles(destination.VaultRoles, destination.CredentialFiles)
	if err != nil {
		return nil, fmt.Errorf("ClickHouse destination %q credentials: %w", name, err)
	}
	common := config{name: name, upstream: destination.NativeUpstream, httpUpstream: httpUpstream, credentialRoles: roles, handshakeTimeout: 10 * time.Second, retryAttempts: 3, retryDelay: 500 * time.Millisecond, httpRetryBodyBytes: 1 << 20, capability: destination.Capability}
	configured := make([]backend.Backend, 0, 2)
	if native {
		configured = append(configured, protocolBackend{common, destination.NativePort, false})
	}
	if http {
		configured = append(configured, protocolBackend{common, destination.HTTPPort, true})
	}
	return configured, nil
}

type protocolBackend struct {
	config
	port uint16
	http bool
}

func (cfg protocolBackend) ListenPort() uint16 { return cfg.port }

func (cfg protocolBackend) Serve(ctx context.Context, listener net.Listener, network *gatewaytailnet.Network, logger *slog.Logger) error {
	logger = logger.With("destination", cfg.name)
	gateway := &Gateway{
		config:    cfg.config,
		Authorize: (&gatewaytailnet.Authorizer{Client: network.Client, Service: network.Service, Capability: cfg.capability, Roles: cfg.credentialRoles}).Authorize,
		Dial:      (&net.Dialer{Timeout: cfg.handshakeTimeout}).DialContext,
		Logger:    logger,
	}
	if cfg.http {
		logger.Info("ClickHouse HTTP destination ready", "port", cfg.port)
		return gateway.ServeHTTP(ctx, listener)
	}
	logger.Info("ClickHouse native destination ready", "port", cfg.port)
	return gateway.Serve(ctx, listener)
}
