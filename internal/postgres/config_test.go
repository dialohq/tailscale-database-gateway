package postgres

import (
	"testing"

	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
	vaultcredentials "github.com/dialohq/tailscale-database-gateway/internal/credentials/vault"
)

func TestConfigureDestinations(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("VAULT_TOKEN_ROLE", "postgres-gateway-database")
	configured, err := New("app-db-ro", gatewayconfig.Destination{Backend: "postgres", Port: 5432, Upstream: "postgresql://db-ro:5432/postgres", Capability: "example.com/cap/postgres-ro", VaultRoles: map[string]vaultcredentials.Role{"readonly": {Policy: "postgres-gateway-readonly", Path: "postgres/creds/readonly"}}})
	if err != nil {
		t.Fatal(err)
	}
	read := configured.(destination)
	if read.name != "app-db-ro" || read.capability != "example.com/cap/postgres-ro" || read.roles["readonly"] == nil {
		t.Fatalf("destinations = %#v", configured)
	}
}

func TestConfigureDestinationsRejectsInvalidValues(t *testing.T) {
	valid := func() gatewayconfig.Destination {
		return gatewayconfig.Destination{Backend: "postgres", Port: 5432, Upstream: "postgres://db/app", Capability: "example.com/cap/postgres-ro", CredentialFiles: map[string]string{"readonly": "/readonly.json"}}
	}
	tests := map[string]gatewayconfig.Destination{
		"invalid port":     func() gatewayconfig.Destination { value := valid(); value.Port = 0; return value }(),
		"invalid upstream": func() gatewayconfig.Destination { value := valid(); value.Upstream = "db:5432"; return value }(),
		"upstream secret": func() gatewayconfig.Destination {
			value := valid()
			value.Upstream = "postgres://user:secret@db/app"
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
			if _, err := New("app-db-ro", configured); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}
