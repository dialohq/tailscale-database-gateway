# Tailscale ClickHouse gateway

This service lets a human or tagged machine on the tailnet run:

```console
clickhouse-client --host clickhouse --user readonly
```

It also exposes ClickHouse HTTP on the same tailnet name:

```console
curl 'http://clickhouse:8123/?user=readonly&query=SELECT%20currentUser()'
```

The client uses the ClickHouse username to select one of its granted roles but
does not need a database password. For the native protocol, the gateway
replaces only the username and password fields in the initial ClickHouse
`ClientHello`; all later bytes pass through unchanged. HTTP accepts the role in
Basic auth, `X-ClickHouse-User`, or the `user` URL parameter. If several forms
are present they must agree. The reverse proxy removes all client credentials,
preserves other HTTP headers, and supplies the minted upstream username and
password. When configured with `TS_SERVICE_NAME`, both listeners are layer-4 endpoints
of the selected Tailscale Service. Each connection carries its original tailnet address in a
required PROXY v2 header; the gateway resolves it with Tailscale LocalAPI
`WhoIsForService` and uses the same grant-to-role authorization. The local
listener receiving that header is private to the embedded Tailscale process.

Human nodes must have a Tailscale user profile; tagged machines must have a
Tailscale hostname. Both require a matching ClickHouse application grant.
Tagged machines are identified by their hostname and stable node ID, even if
Tailscale also returns a user profile.

## Group-to-role authorization

Tailscale evaluates group membership and source tags in the tailnet policy. A
grant attaches one or more ClickHouse roles to the destination's configured
capability; the gateway reads them from `WhoIs.CapMap` and requires the
client-selected role to be both granted and configured. It never needs to
duplicate the tailnet's user or group membership locally.

For example:

```json
{
  "grants": [
    {
      "src": ["group:clickhouse-readers"],
      "dst": ["svc:database-gateway"],
      "ip": ["tcp:9000", "tcp:8123"],
      "app": {
        "example.com/cap/clickhouse": [{ "role": "readonly" }]
      }
    },
    {
      "src": ["group:clickhouse-admins"],
      "dst": ["svc:database-gateway"],
      "ip": ["tcp:9000", "tcp:8123"],
      "app": {
        "example.com/cap/clickhouse": [{ "role": "admin" }]
      }
    }
  ]
}
```

To allow a machine, add a grant with its source tag and the same destination,
ports, and application capability, for example:

```json
{
  "src": ["tag:database-client"],
  "dst": ["svc:database-gateway"],
  "ip": ["tcp:9000", "tcp:8123"],
  "app": {
    "example.com/cap/clickhouse": [{ "role": "readonly" }]
  }
}
```

IPAM can independently restrict network reachability. A peer with network
access but no matching application grant is denied.

Each ClickHouse destination maps its client-selectable roles to exact Vault
policies and dynamic credential paths:

```json
{
  "admin": {
    "policy": "clickhouse-gateway-admin",
    "path": "clickhouse/creds/admin"
  },
  "readonly": {
    "policy": "clickhouse-gateway-readonly",
    "path": "clickhouse/creds/readonly"
  }
}
```

If overlapping groups grant several roles, the client chooses explicitly with
`--user`, Basic auth, `X-ClickHouse-User`, or the `user` URL parameter. Missing,
conflicting, ungranted, and unconfigured roles are rejected; there is no role
precedence or fallback.

## Temporary users and audit identity

For every native connection, the gateway asks the local Vault Proxy
to mint a constrained child token. Its display name contains a sanitized
Tailscale login plus a hash of the exact login. Vault's ClickHouse username
template combines that display name, the dynamic role, and a random suffix,
producing native database names such as
`token-ts-alice-example-com-ab12cd34_readonly_x7k2p9qz`.

For tagged machines, the display name uses the sanitized Tailscale hostname
and its hash instead of the human login. The same username template adds the
role and random suffix.

The child token also records the exact `tailscale_login` (hostname for tagged
machines), `tailscale_node`, `tailscale_node_id`, and `gateway_role` as
metadata in Vault's audit log. Native tokens also carry a random
`gateway_connection` audit ID, which makes each token-creation request unique
to the Proxy cache. The token receives only the policy explicitly configured
for the selected role, which can read only that role's dynamic credential
path. Vault Proxy's broker token can call only the fixed child-token creation
endpoint; it cannot read database credentials itself.

Vault Proxy caches and renews every child token and database lease. When a
native client disconnects, the gateway revokes that connection's child token
through the Proxy; Vault revokes its database lease and the Proxy evicts both
cache entries. A connection is not capped by an application-defined TTL: the
Proxy keeps its renewable token and lease healthy for as long as the connection
is alive.

ClickHouse HTTP has no durable client connection, so identical token and
credential requests remain reusable in Vault Proxy's cache per Tailscale identity,
stable node ID, and selected role. A normal HTTP response does not revoke the
shared token. An upstream authentication failure revokes it through the Proxy,
evicting the rejected token and credentials before a safe replay. Vault's token
and database-role configuration remains the authority for cache lifetime and
maximum lease duration.

Provision the underlying `clickhouse_gateway_readonly` and
`clickhouse_gateway_admin` roles in your database. Vault's `readonly` and `admin` dynamic roles
create temporary users, grant the corresponding Atlas-managed role, and make
it the user's default.

For local development, use `credential_files` instead of `vault_roles`, with
strict JSON files containing
`{"username":"generated-user","password":"generated-password"}`. The two
configuration modes are mutually exclusive.

The gateway also holds the first upstream handshake response: if ClickHouse
returns an exception, it reconnects, reloads the selected file, and replays the
hello up to three times. The client sees only the successful server hello or
the last failure.

HTTP authentication failures evict the rejected cache entry and are retried
with fresh credentials when the request has no body or has a known body no
larger than 1 MiB. Larger or streaming bodies are forwarded once to avoid
buffering uploads or accidentally replaying a partial insert; their next
request still loads the latest credentials.

## Configuration

`DATABASE_GATEWAY_CONFIG_FILE` points to the strict, 1 MiB-bounded JSON document
shared by all database backends. The top-level keys are destination names and
each value selects its backend and Tailscale capability. A ClickHouse
destination may expose native, HTTP, or both; each enabled protocol requires
both its port and upstream. Capabilities must be unique across destinations,
so the grant needs only the role:

```json
{
  "analytics": {
    "backend": "clickhouse",
    "native_port": 9000,
    "native_upstream": "clickhouse:9000",
    "http_port": 8123,
    "http_upstream": "http://clickhouse:8123",
    "capability": "example.com/cap/clickhouse",
    "vault_roles": {
      "readonly": {"policy":"clickhouse-gateway-readonly","path":"clickhouse/creds/readonly"}
    }
  },
  "app-db-ro": {
    "backend": "postgres",
    "port": 5432,
    "upstream": "postgresql://db-ro:5432/postgres?sslmode=verify-full&sslrootcert=/ca.crt",
    "capability": "example.com/cap/postgres-ro",
    "vault_roles": {
      "readonly": {"policy":"postgres-gateway-readonly","path":"postgres/creds/readonly"}
    }
  }
}
```

| Variable | Requirement |
| --- | --- |
| `DATABASE_GATEWAY_CONFIG_FILE` | required path to the unified JSON configuration |
| `VAULT_ADDR` | required in Vault mode; local or HTTPS remote Vault Proxy address |
| `VAULT_TOKEN_ROLE` | required in Vault mode |
| `TS_HOSTNAME` | required |
| `TS_ADVERTISE_TAGS` | required and nonempty when `TS_SERVICE_NAME` is set; optional in direct-node mode |
| `TS_STATE_DIR` | required |
| `TS_SERVICE_NAME` | optional; unset selects direct-node mode |
| `TS_CONTROL_URL` | optional; unset uses Tailscale's control server |
| `TS_AUTHKEY_FILE` | optional; path to a Tailscale auth key, preferred over `TS_AUTHKEY` when set |

When `TS_AUTHKEY_FILE` is unset, `tsnet` reads `TS_AUTHKEY` or
`TS_CLIENT_SECRET` using its normal precedence.
The gateway joins the tailnet using its own Tailscale auth key. In Service mode,
set `TS_ADVERTISE_TAGS` to a tag permitted to advertise the service and approve
the advertisement in your tailnet policy. Multiple gateway processes may
advertise the same Service.

Create the `svc:database-gateway` Service once in the Tailscale control plane with
endpoints `tcp:9000` and `tcp:8123`. Its host advertisements can be approved
automatically in the tailnet policy:

```json
{
  "autoApprovers": {
    "services": {
      "svc:database-gateway": ["tag:database-gateway"]
    }
  }
}
```

The grants above remain the authority for which humans and tagged machines can
reach the Service and which ClickHouse roles they may select. Tailscale
clients should be version 1.94 or newer so Service routes work without
enabling `accept-routes`.

Vault Proxy must be authenticated with a narrowly scoped broker policy, as
described in [setup](setup.md). The gateway sends requests to the Proxy and
does not inherit ambient `VAULT_TOKEN` or `VAULT_NAMESPACE` values.

Leaving `TS_SERVICE_NAME` empty retains direct node listeners for local tests
and Headscale, which does not yet implement Tailscale Services. When using a
Headscale pre-auth key that already assigns `tag:database-gateway`, set
`TS_ADVERTISE_TAGS` to an explicit empty value. Headscale rejects client-side
advertised tags during pre-auth-key registration, including tags already on
the key.

The listeners are not TLS-wrapped because their transport is already inside
Tailscale's WireGuard tunnel. Do not pass `--secure` to `clickhouse-client` and
use `http://`, not `https://`, for these tailnet endpoints.

## Tests

The default suite is hermetic and covers native-protocol parsing, both auth
rewriters, identity rejection, HTTP credential smuggling, arbitrary native
post-handshake traffic, role selection, and safe retry behavior:

```console
nix develop
go test -race ./...
```

The integration script starts a temporary ClickHouse server and exercises
both the real `clickhouse-client` and ClickHouse HTTP through the gateway:

```console
./scripts/test-clickhouse.sh -v
```
