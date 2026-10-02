# Setup

The gateway trusts Tailscale for connection identity and application grants,
Vault for short-lived database credentials, and each database for SQL privileges.
The gateway does not manage tailnet policy or provision Vault itself.

## Tailscale

Set `TS_HOSTNAME` and a writable `TS_STATE_DIR`. Use `TS_AUTHKEY_FILE` to supply
an auth key without putting it in the gateway configuration. Direct-node mode
listens on this node's tailnet addresses. Service mode additionally requires
`TS_SERVICE_NAME` (for example `svc:database-gateway`) and `TS_ADVERTISE_TAGS`.
Create the Service and approve its advertisements in the Tailscale control plane.

Each destination requires both network reachability and an application grant
matching its configured capability and the client-selected role. For example:

```json
{
  "src": ["tag:database-client"],
  "dst": ["svc:database-gateway"],
  "ip": ["tcp:5432"],
  "app": {
    "example.com/cap/postgres": [{"role": "readonly"}]
  }
}
```

Use a group source for human access. Tagged machines use their Tailscale
computed device name as their login; human nodes use their user-profile login.

## Vault

1. Configure the PostgreSQL or ClickHouse database secrets engine with an
   account able to create and revoke temporary logins. Create dynamic roles
   such as `readonly`, with the SQL privileges appropriate to your deployment.
2. Give each gateway role a Vault policy allowing only its credential endpoint
   plus `auth/token/revoke-self`, `auth/token/renew-self`, and `sys/leases/renew`.
3. Configure a renewable child-token role with these policies as its allowed
   policies and no default policy. Set its name in `VAULT_TOKEN_ROLE`.
4. Run Vault Proxy with auto-auth, caching, and `api_proxy.use_auto_auth_token =
   true`. Its broker policy should allow the chosen `auth/token/create/<role>`
   endpoint. Keep its listener private and set `VAULT_ADDR` to that listener.
   Configure upstream TLS and auto-auth for your environment.
5. Map each gateway role to its exact policy and credential path in
   `DATABASE_GATEWAY_CONFIG_FILE`, following [the example](../examples/config.json).

To attribute database usernames to callers, configure the database connection's
Vault username template to include the token display name and role, for example:

```text
{{ printf "%s_%s_%s" (.DisplayName | truncate 32) (.RoleName | truncate 16) (random 8) | lowercase | truncate 63 }}
```

The gateway supplies a sanitized login and hash as the token display name.
Exact login, node name, stable node ID, and role are recorded as token metadata.
Native connections receive distinct child tokens, revoked on disconnect.
HTTP requests can reuse credentials through Vault Proxy's cache. Configure
renewal and maximum lease durations according to your operational requirements.

## File credentials

For development or an external credential manager, replace `vault_roles` with:

```json
"credential_files": {"readonly": "/path/to/readonly.json"}
```

The referenced file contains `{"username":"existing-user","password":"..."}`.
This provider does not create database users. Tailscale authorization still
applies. Only one credential source may be configured per destination.
