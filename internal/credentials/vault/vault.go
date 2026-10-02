package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	"github.com/hashicorp/vault/api"
)

type Config struct {
	Address   string
	TokenRole string
	Roles     map[string]Role
}

type Role struct {
	Policy string `json:"policy"`
	Path   string `json:"path"`
}

type vaultCredentials struct {
	config Config
	proxy  *api.Client
}

func New(config Config) (credentials.Roles, error) {
	// MaxRetries remains zero because child-token creation is not idempotent.
	client, err := api.NewClient(&api.Config{
		Address:          config.Address,
		DisableRedirects: true,
		Timeout:          10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("configure Vault Proxy client: %w", err)
	}
	// NewClient reads VAULT_TOKEN and VAULT_NAMESPACE; leave authentication to
	// the Proxy's scoped auto-auth identity instead of inheriting ambient access.
	client.ClearToken()
	client.ClearNamespace()
	vault := &vaultCredentials{
		config: config,
		proxy:  client,
	}
	return credentials.NewRoles(config.Roles, vault.load), nil
}

func (p *vaultCredentials) load(ctx context.Context, identity credentials.Identity, role string, selected Role, reusable bool) (credentials.Credentials, error) {
	child, err := p.mintChildClient(ctx, identity, role, selected, reusable)
	if err != nil {
		return credentials.Credentials{}, err
	}
	loaded, err := p.readCredentials(ctx, role, selected, child)
	if err != nil {
		return credentials.Credentials{}, p.rejectToken(child, err)
	}
	revoke := sync.OnceValue(func() error { return p.revokeToken(child) })
	loaded.Complete = func(rejected bool) error {
		if reusable && !rejected {
			return nil
		}
		return revoke()
	}
	return loaded, nil
}

func (p *vaultCredentials) mintChildClient(ctx context.Context, identity credentials.Identity, role string, selected Role, reusable bool) (*api.Client, error) {
	// Clone before minting so a clone failure cannot leak a new child token.
	child, err := p.proxy.Clone()
	if err != nil {
		return nil, fmt.Errorf("prepare Vault child client: %w", err)
	}
	// Clone reloads ambient VAULT_TOKEN and VAULT_NAMESPACE from its copied
	// configuration. The child must start unauthenticated in the Proxy namespace.
	child.ClearToken()
	child.ClearNamespace()
	metadata := map[string]string{
		"tailscale_login":   identity.LoginName,
		"tailscale_node":    identity.NodeName,
		"tailscale_node_id": identity.NodeID,
		"gateway_role":      role,
	}
	if !reusable {
		metadata["gateway_connection"] = rand.Text()
	}
	renewable := true
	request := &api.TokenCreateRequest{
		DisplayName:     auditSlug(identity.LoginName),
		Policies:        []string{selected.Policy},
		Metadata:        metadata,
		Renewable:       &renewable,
		NoDefaultPolicy: true,
	}
	secret, err := p.proxy.Auth().Token().CreateWithRoleWithContext(ctx, request, p.config.TokenRole)
	if err != nil {
		return nil, fmt.Errorf("create Vault child token: %w", cleanVaultError(err))
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return nil, fmt.Errorf("create Vault child token: response token is empty")
	}
	child.SetToken(secret.Auth.ClientToken)
	if secret.Auth.LeaseDuration <= 0 || !secret.Auth.Renewable {
		return nil, p.rejectToken(child, fmt.Errorf("create Vault child token: response lease is invalid or not renewable"))
	}
	return child, nil
}

func (p *vaultCredentials) rejectToken(child *api.Client, reason error) error {
	if err := p.revokeToken(child); err != nil {
		return fmt.Errorf("%w; cleanup failed: %v", reason, err)
	}
	return reason
}

func (p *vaultCredentials) readCredentials(ctx context.Context, role string, selected Role, child *api.Client) (credentials.Credentials, error) {
	response, err := child.Logical().ReadWithContext(ctx, selected.Path)
	if err != nil {
		return credentials.Credentials{}, fmt.Errorf("read Vault credentials for role %q: %w", role, cleanVaultError(err))
	}
	// Normalize an absent secret so incomplete responses share one rejection path.
	if response == nil {
		response = &api.Secret{}
	}
	username, _ := response.Data["username"].(string)
	password, _ := response.Data["password"].(string)
	if username == "" || password == "" {
		return credentials.Credentials{}, fmt.Errorf("read Vault credentials for role %q: response credentials are incomplete", role)
	}
	if response.LeaseID == "" || response.LeaseDuration <= 0 || !response.Renewable {
		return credentials.Credentials{}, fmt.Errorf("read Vault credentials for role %q: response lease is incomplete or not renewable", role)
	}
	return credentials.Credentials{Username: username, Password: password}, nil
}

func (p *vaultCredentials) revokeToken(child *api.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := child.Auth().Token().RevokeSelfWithContext(ctx, "")
	if err != nil {
		return fmt.Errorf("revoke Vault child token: %w", cleanVaultError(err))
	}
	return nil
}

func cleanVaultError(err error) error {
	var response *api.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("Vault returned HTTP %d", response.StatusCode)
	}
	return err
}

func auditSlug(login string) string {
	digest := sha256.Sum256([]byte(login))
	hash := hex.EncodeToString(digest[:4])
	var slug strings.Builder
	lastSeparator := false
	for _, character := range strings.ToLower(login) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			slug.WriteRune(character)
			lastSeparator = false
		} else if slug.Len() > 0 && !lastSeparator {
			slug.WriteByte('_')
			lastSeparator = true
		}
		if slug.Len() >= 24 {
			break
		}
	}
	name := strings.Trim(slug.String(), "_")
	if name == "" {
		name = "user"
	}
	return "ts_" + name + "_" + hash
}
