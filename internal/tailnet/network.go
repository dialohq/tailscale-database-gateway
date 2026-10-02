package tailnet

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
	"github.com/pires/go-proxyproto"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

type Network struct {
	server  *tsnet.Server
	Client  WhoIsClient
	Service tailcfg.ServiceName
	Timeout time.Duration
}

func Open(ctx context.Context, logger *slog.Logger) (*Network, error) {
	hostname, err := gatewayconfig.RequiredEnv("TS_HOSTNAME")
	if err != nil {
		return nil, err
	}
	stateDir, err := gatewayconfig.RequiredEnv("TS_STATE_DIR")
	if err != nil {
		return nil, err
	}
	serviceName := os.Getenv("TS_SERVICE_NAME")
	service := tailcfg.AsServiceName(serviceName)
	if serviceName != "" && service == "" {
		return nil, fmt.Errorf("TS_SERVICE_NAME must be a valid Tailscale Service name")
	}
	tags := gatewayconfig.CommaSeparated(os.Getenv("TS_ADVERTISE_TAGS"))
	if service != "" && len(tags) == 0 {
		return nil, fmt.Errorf("TS_ADVERTISE_TAGS is required when TS_SERVICE_NAME is set")
	}
	var authKey string
	if file := strings.TrimSpace(os.Getenv("TS_AUTHKEY_FILE")); file != "" {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read TS_AUTHKEY_FILE: %w", err)
		}
		authKey = strings.TrimSpace(string(contents))
		if authKey == "" {
			return nil, fmt.Errorf("TS_AUTHKEY_FILE is empty")
		}
	}
	server := &tsnet.Server{Hostname: hostname, Dir: stateDir, AuthKey: authKey, Ephemeral: true, AdvertiseTags: tags, UserLogf: func(format string, args ...any) {
		logger.Info("tsnet", "message", fmt.Sprintf(format, args...))
	}}
	if _, err := server.Up(ctx); err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("connect database gateway to tailnet: %w", err)
	}
	client, err := server.LocalClient()
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("create Tailscale local client: %w", err)
	}
	return &Network{server: server, Client: client, Service: service, Timeout: 10 * time.Second}, nil
}

func (network *Network) Listen(port uint16) (net.Listener, error) {
	if network.Service == "" {
		return network.server.Listen("tcp", fmt.Sprintf(":%d", port))
	}
	listener, err := network.server.ListenService(string(network.Service), tsnet.ServiceModeTCP{Port: port, PROXYProtocolVersion: 2})
	if err != nil {
		return nil, err
	}
	return &proxyproto.Listener{Listener: listener, ReadHeaderTimeout: network.Timeout, ConnPolicy: func(connection proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
		if upstream, ok := connection.Upstream.(*net.TCPAddr); ok && upstream.IP.IsLoopback() {
			return proxyproto.REQUIRE, nil
		}
		return 0, proxyproto.ErrInvalidUpstream
	}}, nil
}

func (network *Network) Close() error { return network.server.Close() }
