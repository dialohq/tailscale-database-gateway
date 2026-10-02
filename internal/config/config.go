package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	vaultcredentials "github.com/dialohq/tailscale-database-gateway/internal/credentials/vault"
	"tailscale.com/tailcfg"
)

var vaultNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type Destination struct {
	Backend         string                           `json:"backend"`
	NativePort      uint16                           `json:"native_port,omitempty"`
	NativeUpstream  string                           `json:"native_upstream,omitempty"`
	HTTPPort        uint16                           `json:"http_port,omitempty"`
	HTTPUpstream    string                           `json:"http_upstream,omitempty"`
	Port            uint16                           `json:"port,omitempty"`
	Upstream        string                           `json:"upstream,omitempty"`
	Capability      tailcfg.PeerCapability           `json:"capability"`
	VaultRoles      map[string]vaultcredentials.Role `json:"vault_roles,omitempty"`
	CredentialFiles map[string]string                `json:"credential_files,omitempty"`
}

func LoadFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open database gateway config file: %w", err)
	}
	defer file.Close()
	const maxBytes = 1 << 20
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read database gateway config file: %w", err)
	}
	if len(contents) > maxBytes {
		return fmt.Errorf("database gateway config file exceeds %d bytes", maxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode database gateway config file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode database gateway config file: trailing JSON value")
	}
	return nil
}

func CredentialRoles(vaultRoles map[string]vaultcredentials.Role, files map[string]string) (credentials.Roles, error) {
	if (len(vaultRoles) == 0) == (len(files) == 0) {
		return nil, fmt.Errorf("configure exactly one Vault or file credential source")
	}
	if len(files) > 0 {
		for role, path := range files {
			if !vaultNamePattern.MatchString(role) || strings.TrimSpace(path) == "" {
				return nil, fmt.Errorf("file credential roles and paths must be valid and nonempty")
			}
		}
		return credentials.NewFileRoles(files), nil
	}
	address, err := RequiredEnv("VAULT_ADDR")
	if err != nil {
		return nil, err
	}
	tokenRole, err := RequiredEnv("VAULT_TOKEN_ROLE")
	if err != nil {
		return nil, err
	}
	config := vaultcredentials.Config{Address: address, TokenRole: tokenRole, Roles: vaultRoles}
	if err := validateVaultConfig(config); err != nil {
		return nil, err
	}
	return vaultcredentials.New(config)
}

func validateVaultConfig(config vaultcredentials.Config) error {
	address, err := url.Parse(config.Address)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Host == "" || address.User != nil || (address.Path != "" && address.Path != "/") || address.RawQuery != "" || address.Fragment != "" {
		return fmt.Errorf("Vault Proxy address must be an http or https origin without credentials, path, query, or fragment")
	}
	proxyIP := net.ParseIP(address.Hostname())
	if address.Scheme == "http" && !strings.EqualFold(address.Hostname(), "localhost") && (proxyIP == nil || !proxyIP.IsLoopback()) {
		return fmt.Errorf("remote Vault Proxy address must use https")
	}
	if !vaultNamePattern.MatchString(config.TokenRole) {
		return fmt.Errorf("Vault token role contains unsupported characters")
	}
	for role, configured := range config.Roles {
		if !vaultNamePattern.MatchString(role) {
			return fmt.Errorf("Vault role %q contains unsupported characters", role)
		}
		if !vaultNamePattern.MatchString(configured.Policy) {
			return fmt.Errorf("Vault policy for role %q contains unsupported characters", role)
		}
		if !validVaultPath(configured.Path) {
			return fmt.Errorf("Vault credential path for role %q is invalid", role)
		}
	}
	return nil
}

func validVaultPath(path string) bool {
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.ContainsAny(path, `?#\\%`) {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func RequiredEnv(name string) (string, error) {
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func CommaSeparated(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}
