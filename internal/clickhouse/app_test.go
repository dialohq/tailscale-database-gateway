package clickhouse

import (
	"testing"

	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
	vaultcredentials "github.com/dialohq/tailscale-database-gateway/internal/credentials/vault"
)

func TestConfigureDestinations(t *testing.T) {
	configured, err := New("analytics", gatewayconfig.Destination{Backend: "clickhouse", NativePort: 9000, NativeUpstream: "clickhouse:9000", HTTPPort: 8123, HTTPUpstream: "http://clickhouse:8123", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/credentials/readonly.json"}})
	if err != nil {
		t.Fatal(err)
	}
	analytics := configured[0].(protocolBackend)
	analyticsHTTP := configured[1].(protocolBackend)
	if len(configured) != 2 || analytics.name != "analytics" || analytics.http || !analyticsHTTP.http || analytics.capability != "example.com/cap/clickhouse" || analytics.credentialRoles["readonly"] == nil {
		t.Fatalf("configured = %#v", configured)
	}
}

func TestConfigureDestinationWithOneProtocol(t *testing.T) {
	for name, destination := range map[string]gatewayconfig.Destination{
		"native": {Backend: "clickhouse", NativePort: 9000, NativeUpstream: "clickhouse:9000", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/credentials/readonly.json"}},
		"HTTP":   {Backend: "clickhouse", HTTPPort: 8123, HTTPUpstream: "http://clickhouse:8123", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/credentials/readonly.json"}},
	} {
		t.Run(name, func(t *testing.T) {
			configured, err := New("analytics", destination)
			if err != nil || len(configured) != 1 {
				t.Fatalf("configured = %#v, error = %v", configured, err)
			}
		})
	}
}

func TestConfigureDestinationsRejectsInvalidValues(t *testing.T) {
	valid := func() gatewayconfig.Destination {
		return gatewayconfig.Destination{Backend: "clickhouse", NativePort: 9000, NativeUpstream: "clickhouse:9000", HTTPPort: 8123, HTTPUpstream: "http://clickhouse:8123", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/credentials/readonly.json"}}
	}
	tests := map[string]gatewayconfig.Destination{
		"invalid native": func() gatewayconfig.Destination { value := valid(); value.NativeUpstream = "clickhouse"; return value }(),
		"incomplete native": func() gatewayconfig.Destination {
			value := valid()
			value.NativePort = 0
			return value
		}(),
		"invalid HTTP": func() gatewayconfig.Destination {
			value := valid()
			value.HTTPUpstream = "clickhouse:8123"
			return value
		}(),
		"incomplete HTTP": func() gatewayconfig.Destination {
			value := valid()
			value.HTTPPort = 0
			return value
		}(),
		"upstream secret": func() gatewayconfig.Destination {
			value := valid()
			value.HTTPUpstream = "https://user:secret@clickhouse:8123"
			return value
		}(),
		"same ports": func() gatewayconfig.Destination { value := valid(); value.HTTPPort = value.NativePort; return value }(),
		"no protocols": func() gatewayconfig.Destination {
			value := valid()
			value.NativePort, value.NativeUpstream, value.HTTPPort, value.HTTPUpstream = 0, "", 0, ""
			return value
		}(),
		"both credentials": func() gatewayconfig.Destination {
			value := valid()
			value.VaultRoles = map[string]vaultcredentials.Role{"readonly": {}}
			return value
		}(),
		"no credentials": func() gatewayconfig.Destination { value := valid(); value.CredentialFiles = nil; return value }(),
	}
	for name, configured := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New("analytics", configured); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}
