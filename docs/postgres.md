# Tailscale PostgreSQL gateway

The PostgreSQL gateway gives human tailnet users and tagged machines
passwordless access to short-lived database logins. The client-visible user is
a logical role:

```console
psql 'host=database-gateway.example.ts.net port=5432 dbname=app user=readonly'
psql 'host=database-gateway.example.ts.net port=5433 dbname=app user=admin'
```

Each configured destination has its own port, keeping read-only and read-write
reachability independently controllable by Tailscale grants and network
policy. PostgreSQL's startup `user` selects a role; the startup `database`
selects the upstream database. Both are required and there is no fallback.

## Authorization

The gateway resolves the connection with Tailscale `WhoIsForService`. It
accepts human user nodes and tagged machines, and requires one application
grant under the destination's configured capability matching the role.
PostgreSQL and Vault permissions determine which databases that role may
access.

```json
{
  "grants": [
    {
      "src": ["group:postgres-readers"],
      "dst": ["svc:database-gateway"],
      "ip": ["tcp:5432"],
      "app": {
        "example.com/cap/postgres-ro": [
          {"role":"readonly"}
        ]
      }
    },
    {
      "src": ["group:postgres-admins"],
      "dst": ["svc:database-gateway"],
      "ip": ["tcp:5433"],
      "app": {
        "example.com/cap/postgres-rw": [
          {"role":"admin"}
        ]
      }
    }
  ],
  "autoApprovers": {
    "services": {"svc:database-gateway":["tag:database-gateway"]}
  }
}
```

The gateway uses `pgx` for the upstream TLS and SCRAM handshake, then proxies
the native protocol byte-for-byte. Client `SSLRequest` and `GSSENCRequest`
are declined because the client hop is already encrypted by Tailscale.
Configure upstream TLS with `verify-full` and a trusted CA.
Unsafe startup parameters such as `options`, `role`, `replication`, and
`session_authorization` are removed. `application_name` is replaced with
`tailscale:<login>@<node>` for database observability. For tagged machines,
the login is the hostname.

PostgreSQL cancellation keys are bound to the originating Tailscale source IP
and forwarded only to the exact upstream server owning the session. The source
IP comes from tsnet directly, or from the required PROXY v2 header on a
Tailscale Service listener; arbitrary Kubernetes clients cannot supply it.

## Vault credentials

For each native connection the local Vault Proxy mints a constrained child
token containing the Tailscale login (hostname for tagged machines), node,
node ID, selected role, and a random connection identifier. The child can read
only that role's configured dynamic credential endpoint. Vault's PostgreSQL
username template includes the token display name, role, and randomness, so
temporary database role names are attributable in PostgreSQL and Vault audit
logs. For tagged machines, the token display name is derived from the
Tailscale hostname instead of a human login.

The token and its dynamic database lease are revoked when the connection
closes. An upstream `28P01` or `28000` rejection revokes the credential and
retries with a freshly minted one. Other failures are not retried. There is no
local credential cache and no gateway-imposed session lifetime.

## Configuration

`DATABASE_GATEWAY_CONFIG_FILE`, `TS_HOSTNAME`, and `TS_STATE_DIR` are required.
`VAULT_ADDR` and `VAULT_TOKEN_ROLE` are required when any destination uses
`vault_roles`. `TS_SERVICE_NAME` is optional for direct tsnet/Headscale
tests; when set, `TS_ADVERTISE_TAGS` is required. `TS_AUTHKEY_FILE` is
optional.

The unified JSON file is strict and limited to 1 MiB. Its top-level keys name
destinations; every value selects a backend and a capability unique to that
destination. PostgreSQL grants contain only the requested role; database
permissions remain in Vault and PostgreSQL:

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

For local tests, use `credential_files` instead of `vault_roles`; each file
is strict JSON containing `{"username":"...","password":"..."}`. The two
credential sources are mutually exclusive per destination.

## Tests

```console
go test -race ./...
nix develop -c ./scripts/test-postgres.sh
```

The first command covers framing, grant selection, credential lifecycle,
startup sanitization, cancellation packet parsing, and transparent proxying.
The second starts a real PostgreSQL server and uses `pgx` to verify credential
rewriting, audit naming, live query cancellation, continued use of the canceled
connection, upstream session cleanup, and transparent query proxying.
