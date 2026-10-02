package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"

	"github.com/dialohq/tailscale-database-gateway/internal/backend"
	"github.com/dialohq/tailscale-database-gateway/internal/clickhouse"
	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
	"github.com/dialohq/tailscale-database-gateway/internal/postgres"
	"github.com/dialohq/tailscale-database-gateway/internal/tailnet"
)

var destinationNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func Run(ctx context.Context, logger *slog.Logger) error {
	path, err := gatewayconfig.RequiredEnv("DATABASE_GATEWAY_CONFIG_FILE")
	if err != nil {
		return err
	}
	var destinations map[string]gatewayconfig.Destination
	if err := gatewayconfig.LoadFile(path, &destinations); err != nil {
		return err
	}
	backends, err := configure(destinations)
	if err != nil {
		return err
	}
	network, err := tailnet.Open(ctx, logger)
	if err != nil {
		return fmt.Errorf("configure tailnet: %w", err)
	}
	defer network.Close()
	return serve(ctx, backends, network.Listen, network, logger)
}

func serve(ctx context.Context, backends []backend.Backend, listen func(uint16) (net.Listener, error), network *tailnet.Network, logger *slog.Logger) error {
	listeners := make([]net.Listener, 0, len(backends))
	for _, configured := range backends {
		listener, err := listen(configured.ListenPort())
		if err != nil {
			return fmt.Errorf("listen on port %d: %w", configured.ListenPort(), err)
		}
		defer listener.Close()
		listeners = append(listeners, listener)
	}
	serveContext, stop := context.WithCancel(ctx)
	defer stop()
	serveErrors := make(chan error, len(backends))
	for i, configured := range backends {
		go func() {
			serveErrors <- configured.Serve(serveContext, listeners[i], network, logger)
		}()
	}
	first := <-serveErrors
	stop()
	for range len(backends) - 1 {
		first = errors.Join(first, <-serveErrors)
	}
	return first
}

func configure(destinations map[string]gatewayconfig.Destination) ([]backend.Backend, error) {
	if len(destinations) == 0 {
		return nil, fmt.Errorf("at least one database destination is required")
	}
	var backends []backend.Backend
	for name, destination := range destinations {
		if !destinationNamePattern.MatchString(name) {
			return nil, fmt.Errorf("database destination name %q is invalid", name)
		}
		capability := string(destination.Capability)
		if capability == "" || strings.TrimSpace(capability) != capability {
			return nil, fmt.Errorf("database destination %q has an invalid capability", name)
		}
		switch destination.Backend {
		case "clickhouse":
			configured, err := clickhouse.New(name, destination)
			if err != nil {
				return nil, err
			}
			backends = append(backends, configured...)
		case "postgres":
			configured, err := postgres.New(name, destination)
			if err != nil {
				return nil, err
			}
			backends = append(backends, configured)
		default:
			return nil, fmt.Errorf("database destination %q has unsupported backend %q", name, destination.Backend)
		}
	}
	ports := make(map[uint16]bool)
	for _, configured := range backends {
		port := configured.ListenPort()
		if ports[port] {
			return nil, fmt.Errorf("database port %d is configured more than once", port)
		}
		ports[port] = true
	}
	return backends, nil
}
