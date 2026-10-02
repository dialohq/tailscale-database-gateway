package postgres

import (
	"fmt"
	"net/url"

	"github.com/dialohq/tailscale-database-gateway/internal/backend"
	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	"tailscale.com/tailcfg"
)

type destination struct {
	name       string
	port       uint16
	capability tailcfg.PeerCapability
	roles      credentials.Roles
	connector  Connector
}

func New(name string, configured gatewayconfig.Destination) (backend.Backend, error) {
	if configured.Port == 0 || configured.NativePort != 0 || configured.NativeUpstream != "" || configured.HTTPPort != 0 || configured.HTTPUpstream != "" {
		return nil, fmt.Errorf("PostgreSQL destination %q has an invalid port or fields for another backend", name)
	}
	upstream, err := url.Parse(configured.Upstream)
	if err != nil || upstream.Scheme != "postgres" && upstream.Scheme != "postgresql" || upstream.Host == "" || upstream.User != nil {
		return nil, fmt.Errorf("PostgreSQL destination %q has an invalid upstream URL", name)
	}
	roles, err := gatewayconfig.CredentialRoles(configured.VaultRoles, configured.CredentialFiles)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL destination %q credentials: %w", name, err)
	}
	connector, err := NewPGXConnector(configured.Upstream)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL destination %q upstream: %w", name, err)
	}
	return destination{name, configured.Port, configured.Capability, roles, connector}, nil
}
