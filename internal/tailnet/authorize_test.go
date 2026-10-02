package tailnet

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const testCapability tailcfg.PeerCapability = "example.com/cap/database"

type testWhoIsClient struct {
	response         *apitype.WhoIsResponse
	requestedService tailcfg.ServiceName
}

func (c *testWhoIsClient) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return c.response, nil
}

func (c *testWhoIsClient) WhoIsForService(_ context.Context, _ string, service tailcfg.ServiceName) (*apitype.WhoIsResponse, error) {
	c.requestedService = service
	return c.response, nil
}

func TestAuthorizeRole(t *testing.T) {
	capabilities := tailcfg.PeerCapMap{
		testCapability: {
			tailcfg.RawMessage(`{"role":"readonly"}`),
			tailcfg.RawMessage(`{"role":" admin "}`),
			tailcfg.RawMessage(`{"role":"readonly"}`),
		},
	}
	for _, role := range []string{"readonly", "admin"} {
		provider, err := authorizeRole(capabilities, role, credentials.Roles{
			role: func(context.Context, credentials.Identity, bool) (credentials.Credentials, error) {
				return credentials.Credentials{Username: role}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := provider(context.Background(), credentials.Identity{}, false)
		if err != nil || loaded.Username != role {
			t.Fatalf("credentials = %#v, error = %v", loaded, err)
		}
	}
	if _, err := authorizeRole(capabilities, "Admin", nil); err == nil {
		t.Fatal("role authorization was not exact")
	}
}

func TestAuthorizeRoleRejectsMissingIdentity(t *testing.T) {
	for name, who := range map[string]*apitype.WhoIsResponse{
		"missing response": nil,
		"missing node":     {},
		"missing hostname": {Node: &tailcfg.Node{Tags: []string{"tag:server"}}, UserProfile: &tailcfg.UserProfile{LoginName: "alice@example.com"}},
		"missing profile":  {Node: &tailcfg.Node{}},
		"empty login":      {Node: &tailcfg.Node{}, UserProfile: &tailcfg.UserProfile{}},
		"blank hostname":   {Node: &tailcfg.Node{Tags: []string{"tag:server"}, ComputedName: " "}},
	} {
		t.Run(name, func(t *testing.T) {
			if who != nil {
				who.CapMap = tailcfg.PeerCapMap{testCapability: {tailcfg.RawMessage(`{"role":"readonly"}`)}}
			}
			authorizer := Authorizer{
				Client: &testWhoIsClient{response: who}, Capability: testCapability,
				Roles: credentials.Roles{"readonly": func(context.Context, credentials.Identity, bool) (credentials.Credentials, error) {
					return credentials.Credentials{}, nil
				}},
			}
			if _, _, err := authorizer.Authorize(context.Background(), "100.64.0.1:1234", "readonly"); err == nil {
				t.Fatal("peer with missing identity was authorized")
			}
		})
	}
}

func TestAuthorizeTaggedMachine(t *testing.T) {
	provider := func(context.Context, credentials.Identity, bool) (credentials.Credentials, error) {
		return credentials.Credentials{Username: "machine-credentials"}, nil
	}
	for _, profile := range []*tailcfg.UserProfile{nil, {LoginName: "owner@example.com"}} {
		who := &apitype.WhoIsResponse{
			Node:        &tailcfg.Node{Tags: []string{"tag:server"}, ComputedName: "batch-worker", StableID: "node-1"},
			UserProfile: profile,
			CapMap:      tailcfg.PeerCapMap{testCapability: {tailcfg.RawMessage(`{"role":"readonly"}`)}},
		}
		authorizer := Authorizer{Client: &testWhoIsClient{response: who}, Capability: testCapability, Roles: credentials.Roles{"readonly": provider, "admin": provider}}
		identity, selected, err := authorizer.Authorize(context.Background(), "100.64.0.1:1234", "readonly")
		if err != nil {
			t.Fatal(err)
		}
		if identity != (credentials.Identity{LoginName: "batch-worker", NodeName: "batch-worker", NodeID: "node-1"}) {
			t.Fatalf("machine identity = %#v", identity)
		}
		loaded, err := selected(context.Background(), identity, false)
		if err != nil || loaded.Username != "machine-credentials" {
			t.Fatalf("credentials = %#v, error = %v", loaded, err)
		}
		if _, _, err := authorizer.Authorize(context.Background(), "100.64.0.1:1234", "admin"); err == nil {
			t.Fatal("machine was authorized for an ungranted role")
		}
		authorizer.Roles = nil
		if _, _, err := authorizer.Authorize(context.Background(), "100.64.0.1:1234", "readonly"); err == nil {
			t.Fatal("machine was authorized for an unconfigured role")
		}
		authorizer.Roles = credentials.Roles{"readonly": provider}
		who.CapMap = nil
		if _, _, err := authorizer.Authorize(context.Background(), "100.64.0.1:1234", "readonly"); err == nil {
			t.Fatal("machine was authorized without a grant")
		}
	}
}

func TestAuthorizeScopesWhoIsToService(t *testing.T) {
	client := &testWhoIsClient{response: &apitype.WhoIsResponse{}}
	authorizer := Authorizer{Client: client, Service: "svc:database"}
	_, _, _ = authorizer.Authorize(context.Background(), "100.64.0.1:1234", "readonly")
	if client.requestedService != authorizer.Service {
		t.Fatalf("WhoIs service = %q, want %q", client.requestedService, authorizer.Service)
	}
}

func TestAuthorizeRoleRejectsMissingGrant(t *testing.T) {
	_, err := authorizeRole(nil, "readonly", nil)
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("error = %v, want missing role grant", err)
	}
}

func TestAuthorizeRoleIgnoresOtherCapability(t *testing.T) {
	_, err := authorizeRole(tailcfg.PeerCapMap{
		"example.com/cap/other": {tailcfg.RawMessage(`{"role":"readonly"}`)},
	}, "readonly", credentials.Roles{"readonly": func(context.Context, credentials.Identity, bool) (credentials.Credentials, error) {
		return credentials.Credentials{}, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("error = %v, want missing role grant", err)
	}
}

func TestAuthorizeRoleRejectsUnconfiguredGrant(t *testing.T) {
	_, err := authorizeRole(tailcfg.PeerCapMap{
		testCapability: {tailcfg.RawMessage(`{"role":"readonly"}`)},
	}, "readonly", nil)
	if err == nil || !strings.Contains(err.Error(), "not granted configured") {
		t.Fatalf("error = %v, want unconfigured role", err)
	}
}

func TestAuthorizeRoleRejectsMalformedGrant(t *testing.T) {
	_, err := authorizeRole(tailcfg.PeerCapMap{
		testCapability: {tailcfg.RawMessage(`{"role":42}`)},
	}, "readonly", nil)
	if err == nil || !strings.Contains(err.Error(), "decode Tailscale role grants") {
		t.Fatalf("error = %v, want malformed role grant", err)
	}
}

func TestAuthorizeRoleRejectsEmptyRole(t *testing.T) {
	_, err := authorizeRole(tailcfg.PeerCapMap{
		testCapability: {tailcfg.RawMessage(`{"role":" "}`)},
	}, "readonly", nil)
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("error = %v, want missing role grant", err)
	}
}

func authorizeRole(capabilities tailcfg.PeerCapMap, role string, configured credentials.Roles) (credentials.Provider, error) {
	authorizer := Authorizer{
		Client: &testWhoIsClient{
			response: &apitype.WhoIsResponse{
				Node:        &tailcfg.Node{ComputedName: "alice-laptop"},
				UserProfile: &tailcfg.UserProfile{LoginName: "alice@example.com"},
				CapMap:      capabilities,
			},
		},
		Capability: testCapability,
		Roles:      configured,
	}
	identity, provider, err := authorizer.Authorize(context.Background(), "100.64.0.1:1234", role)
	if err == nil && identity.LoginName != "alice@example.com" {
		return nil, fmt.Errorf("human login = %q", identity.LoginName)
	}
	return provider, err
}
