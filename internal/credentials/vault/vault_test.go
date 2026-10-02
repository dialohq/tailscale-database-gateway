package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
)

type Identity = credentials.Identity
type Provider = credentials.Provider

type tokenRequest struct {
	DisplayName     string            `json:"display_name"`
	Policies        []string          `json:"policies"`
	Metadata        map[string]string `json:"meta"`
	Renewable       bool              `json:"renewable"`
	NoDefaultPolicy bool              `json:"no_default_policy"`
}

func TestVaultCredentialsCreatesConnectionScopedTokens(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "parent-token-must-come-from-proxy")
	t.Setenv("VAULT_NAMESPACE", "namespace-must-come-from-proxy")
	var mutex sync.Mutex
	var requests []tokenRequest
	var revocations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Vault-Request") != "true" {
			t.Error("request is missing the Vault proxy request marker")
		}
		if request.Header.Get("X-Vault-Namespace") != "" {
			t.Error("request inherited a Vault namespace from the environment")
		}
		switch request.URL.Path {
		case "/v1/auth/token/create/clickhouse-gateway-database":
			if request.Header.Get("X-Vault-Token") != "" {
				t.Errorf("token creation token = %q", request.Header.Get("X-Vault-Token"))
			}
			var body tokenRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mutex.Lock()
			requests = append(requests, body)
			token := "child-" + strconv.Itoa(len(requests))
			mutex.Unlock()
			_, _ = response.Write([]byte(`{"auth":{"client_token":"` + token + `","lease_duration":60,"renewable":true}}`))
		case "/v1/clickhouse/creds/admin":
			token := request.Header.Get("X-Vault-Token")
			_, _ = response.Write([]byte(`{"lease_id":"clickhouse/creds/admin/lease","lease_duration":60,"renewable":true,"data":{"username":"` + token + `","password":"secret"}}`))
		case "/v1/auth/token/revoke-self":
			revocations.Add(1)
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	provider := newVaultTestProvider(t, server.URL)
	identity := Identity{LoginName: "Alice@Example.COM", NodeName: "alice-laptop", NodeID: "node-1"}
	first, err := provider(context.Background(), identity, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider(context.Background(), identity, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Username == second.Username {
		t.Fatalf("connection-scoped loads reused %q", first.Username)
	}
	if err := first.Complete(false); err != nil {
		t.Fatal(err)
	}
	if err := first.Complete(false); err != nil {
		t.Fatal(err)
	}
	if err := second.Complete(false); err != nil {
		t.Fatal(err)
	}
	if revocations.Load() != 2 {
		t.Fatalf("revocations = %d, want 2", revocations.Load())
	}
	if len(requests) != 2 || requests[0].Metadata["gateway_connection"] == "" || requests[0].Metadata["gateway_connection"] == requests[1].Metadata["gateway_connection"] {
		t.Fatalf("connection audit IDs = %q, %q", requests[0].Metadata["gateway_connection"], requests[1].Metadata["gateway_connection"])
	}
	assertTokenAuditRequest(t, requests[0], identity, "admin")
}

func TestVaultCredentialsReusableRequestsDelegateCachingToProxy(t *testing.T) {
	var requests []tokenRequest
	var revocations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/auth/token/create/clickhouse-gateway-database":
			var body tokenRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			requests = append(requests, body)
			_, _ = response.Write([]byte(`{"auth":{"client_token":"cached-child","lease_duration":60,"renewable":true}}`))
		case "/v1/clickhouse/creds/admin":
			_, _ = response.Write([]byte(`{"lease_id":"clickhouse/creds/admin/cached","lease_duration":60,"renewable":true,"data":{"username":"cached-user","password":"secret"}}`))
		case "/v1/auth/token/revoke-self":
			revocations.Add(1)
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	provider := newVaultTestProvider(t, server.URL)
	identity := Identity{LoginName: "alice@example.com", NodeName: "laptop", NodeID: "node-1"}
	first, err := provider(context.Background(), identity, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider(context.Background(), identity, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Complete(false); err != nil {
		t.Fatal(err)
	}
	if err := second.Complete(false); err != nil {
		t.Fatal(err)
	}
	if revocations.Load() != 0 {
		t.Fatalf("normal HTTP release revoked reusable token")
	}
	if len(requests) != 2 || requests[0].Metadata["gateway_connection"] != "" || requests[1].Metadata["gateway_connection"] != "" {
		t.Fatalf("reusable requests unexpectedly contain connection IDs: %#v", requests)
	}
	firstJSON, _ := json.Marshal(requests[0])
	secondJSON, _ := json.Marshal(requests[1])
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("reusable token requests differ:\n%s\n%s", firstJSON, secondJSON)
	}
	if err := first.Complete(true); err != nil {
		t.Fatal(err)
	}
	if revocations.Load() != 1 {
		t.Fatalf("invalidations = %d, want 1", revocations.Load())
	}
}

func TestVaultCredentialsUsesConfiguredProxyAndNames(t *testing.T) {
	var created atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/auth/token/create/custom-broker":
			var body tokenRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			created.Store(len(body.Policies) == 1 && body.Policies[0] == "custom-access-operator")
			_, _ = response.Write([]byte(`{"auth":{"client_token":"custom-child","lease_duration":60,"renewable":true}}`))
		case "/v1/custom/creds/operator":
			_, _ = response.Write([]byte(`{"lease_id":"custom/creds/operator/lease","lease_duration":60,"renewable":true,"data":{"username":"operator-user","password":"secret"}}`))
		case "/v1/auth/token/revoke-self":
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	roles, err := New(Config{
		Address:   server.URL,
		TokenRole: "custom-broker",
		Roles:     map[string]Role{"operator": {Policy: "custom-access-operator", Path: "custom/creds/operator"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := roles["operator"](context.Background(), Identity{LoginName: "alice"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "operator-user" || !created.Load() {
		t.Fatalf("credentials = %#v, configured policy used = %t", credentials, created.Load())
	}
	if err := credentials.Complete(false); err != nil {
		t.Fatal(err)
	}
}

func TestVaultCredentialsRevokesRejectedChildToken(t *testing.T) {
	for _, test := range []struct {
		name              string
		token             string
		renewable         bool
		credentialFailure bool
		revocationFailure bool
		requiredError     string
		secretError       string
	}{
		{name: "credential read failure", token: "do-not-leak", renewable: true, credentialFailure: true, secretError: "secret internal detail"},
		{name: "credential cleanup failure", token: "do-not-leak", renewable: true, credentialFailure: true, revocationFailure: true, requiredError: "cleanup failed", secretError: "secret internal detail"},
		{name: "nonrenewable token", token: "nonrenewable-token", requiredError: "not renewable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var revoked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/auth/token/create/clickhouse-gateway-database":
					_, _ = response.Write([]byte(`{"auth":{"client_token":"` + test.token + `","lease_duration":60,"renewable":` + strconv.FormatBool(test.renewable) + `}}`))
				case "/v1/clickhouse/creds/admin":
					if test.credentialFailure {
						http.Error(response, test.secretError, http.StatusInternalServerError)
					}
				case "/v1/auth/token/revoke-self":
					revoked.Store(request.Header.Get("X-Vault-Token") == test.token)
					if test.revocationFailure {
						http.Error(response, "secret cleanup detail", http.StatusInternalServerError)
						return
					}
					response.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			provider := newVaultTestProvider(t, server.URL)
			_, err := provider(context.Background(), Identity{LoginName: "alice"}, false)
			if err == nil || test.requiredError != "" && !strings.Contains(err.Error(), test.requiredError) || test.secretError != "" && strings.Contains(err.Error(), test.secretError) || strings.Contains(err.Error(), test.token) {
				t.Fatalf("unsafe or missing error: %v", err)
			}
			if !revoked.Load() {
				t.Fatal("child token was not revoked")
			}
		})
	}
}

func TestAuditSlugIsReadableBoundedAndCollisionResistant(t *testing.T) {
	first := auditSlug("Alice+Support@example.com")
	second := auditSlug("Alice-Support@example.com")
	if !strings.HasPrefix(first, "ts_alice_support_example_") || len(first) > 36 || first == second {
		t.Fatalf("audit slugs = %q, %q", first, second)
	}
}

func newVaultTestProvider(t *testing.T, address string) Provider {
	t.Helper()
	roles, err := New(Config{
		Address:   address,
		TokenRole: "clickhouse-gateway-database",
		Roles:     map[string]Role{"admin": {Policy: "clickhouse-gateway-admin", Path: "clickhouse/creds/admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return roles["admin"]
}

func assertTokenAuditRequest(t *testing.T, request tokenRequest, identity Identity, role string) {
	t.Helper()
	if !strings.Contains(request.DisplayName, "alice_example_com") || len(request.Policies) != 1 || request.Policies[0] != "clickhouse-gateway-"+role {
		t.Fatalf("unexpected token request: %#v", request)
	}
	if request.Metadata["tailscale_login"] != identity.LoginName || request.Metadata["tailscale_node"] != identity.NodeName || request.Metadata["tailscale_node_id"] != identity.NodeID || request.Metadata["gateway_role"] != role {
		t.Fatalf("unexpected audit metadata: %#v", request.Metadata)
	}
	if !request.Renewable || !request.NoDefaultPolicy {
		t.Fatalf("unsafe token request: %#v", request)
	}
}
