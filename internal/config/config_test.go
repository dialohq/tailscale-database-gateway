package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	vaultcredentials "github.com/dialohq/tailscale-database-gateway/internal/credentials/vault"
)

func TestLoadFile(t *testing.T) {
	var configured struct {
		Name string `json:"name"`
	}
	if err := LoadFile(writeConfig(t, `{"name":"gateway"}`), &configured); err != nil {
		t.Fatal(err)
	}
	if configured.Name != "gateway" {
		t.Fatalf("configured = %#v", configured)
	}
}

func TestLoadFileRejectsInvalidDocuments(t *testing.T) {
	for name, document := range map[string]string{
		"unknown field": `{"name":"gateway","extra":true}`,
		"trailing JSON": `{"name":"gateway"} {}`,
		"oversized":     strings.Repeat(" ", (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			var configured struct {
				Name string `json:"name"`
			}
			if err := LoadFile(writeConfig(t, document), &configured); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestCredentialRolesConfiguresVault(t *testing.T) {
	var configuredNames bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/auth/token/create/custom-broker":
			var token struct {
				Policies []string `json:"policies"`
			}
			if err := json.NewDecoder(request.Body).Decode(&token); err != nil {
				t.Error(err)
			}
			configuredNames = len(token.Policies) == 1 && token.Policies[0] == "custom-access-operator"
			_, _ = response.Write([]byte(`{"auth":{"client_token":"child","lease_duration":60,"renewable":true}}`))
		case "/v1/custom/creds/operator":
			_, _ = response.Write([]byte(`{"lease_id":"custom/lease","lease_duration":60,"renewable":true,"data":{"username":"operator-user","password":"secret"}}`))
		case "/v1/auth/token/revoke-self":
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("VAULT_ADDR", server.URL)
	t.Setenv("VAULT_TOKEN_ROLE", "custom-broker")

	roles, err := CredentialRoles(map[string]vaultcredentials.Role{"operator": {Policy: "custom-access-operator", Path: "custom/creds/operator"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := roles["operator"](context.Background(), credentials.Identity{LoginName: "alice"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Complete(false)
	if loaded.Username != "operator-user" || !configuredNames {
		t.Fatalf("credentials = %#v, configured names used = %t", loaded, configuredNames)
	}
}

func TestCredentialRolesRejectsUnsafeVaultConfiguration(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("VAULT_TOKEN_ROLE", "database-broker")
	valid := func() map[string]vaultcredentials.Role {
		return map[string]vaultcredentials.Role{"support": {Policy: "support", Path: "clickhouse/creds/support"}}
	}
	for name, configure := range map[string]func(*testing.T, map[string]vaultcredentials.Role){
		"address credentials": func(t *testing.T, _ map[string]vaultcredentials.Role) {
			t.Setenv("VAULT_ADDR", "http://token@example.com")
		},
		"remote cleartext": func(t *testing.T, _ map[string]vaultcredentials.Role) {
			t.Setenv("VAULT_ADDR", "http://proxy.example:8200")
		},
		"token role": func(t *testing.T, _ map[string]vaultcredentials.Role) { t.Setenv("VAULT_TOKEN_ROLE", "../role") },
		"selected role": func(_ *testing.T, roles map[string]vaultcredentials.Role) {
			roles["support/admin"] = roles["support"]
			delete(roles, "support")
		},
		"policy": func(_ *testing.T, roles map[string]vaultcredentials.Role) {
			roles["support"] = vaultcredentials.Role{Policy: "policy/", Path: "clickhouse/creds/support"}
		},
		"credential path": func(_ *testing.T, roles map[string]vaultcredentials.Role) {
			roles["support"] = vaultcredentials.Role{Policy: "support", Path: "../auth/token"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			roles := valid()
			configure(t, roles)
			if _, err := CredentialRoles(roles, nil); err == nil {
				t.Fatal("accepted unsafe Vault configuration")
			}
		})
	}
}

func TestCredentialRolesRequiresOneSource(t *testing.T) {
	files := map[string]string{"readonly": "/readonly.json"}
	vault := map[string]vaultcredentials.Role{"readonly": {Policy: "readonly", Path: "clickhouse/creds/readonly"}}
	if _, err := CredentialRoles(nil, nil); err == nil {
		t.Fatal("accepted no credential source")
	}
	if _, err := CredentialRoles(vault, files); err == nil {
		t.Fatal("accepted two credential sources")
	}
	for _, invalid := range []map[string]string{{"": "/readonly.json"}, {"readonly": ""}} {
		if _, err := CredentialRoles(nil, invalid); err == nil {
			t.Fatal("accepted an invalid file credential source")
		}
	}
}

func TestCredentialRolesRequiresVaultConnection(t *testing.T) {
	vault := map[string]vaultcredentials.Role{"readonly": {Policy: "readonly", Path: "clickhouse/creds/readonly"}}
	for _, variable := range []string{"VAULT_ADDR", "VAULT_TOKEN_ROLE"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
			t.Setenv("VAULT_TOKEN_ROLE", "database-broker")
			t.Setenv(variable, "")
			if _, err := CredentialRoles(vault, nil); err == nil || !strings.Contains(err.Error(), variable+" is required") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
