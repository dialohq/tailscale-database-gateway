# Tailscale database gateway

Passwordless PostgreSQL and ClickHouse access using Tailscale identities and
Vault-issued database credentials. Both human users and tagged machines are
supported; access is controlled by Tailscale application grants.

This project builds one database gateway process. Each database
backend owns its protocol and configuration while sharing Tailscale identity,
Vault credentials, connection forwarding, and process lifecycle:

```text
cmd/
  database-gateway/       single executable entrypoint
internal/
  backend/                small contract implemented by every database backend
  database/               composes and serves the configured backends
  clickhouse/             ClickHouse native and HTTP handling
  postgres/               PostgreSQL startup/authentication and TCP forwarding
  credentials/            generic role selection, lifecycle, and file provider
    vault/                Vault Proxy token and dynamic credential implementation
  tailnet/                shared human and machine identity resolution
  config/                 strict shared file and environment parsing
  service/                shared process and health-server lifecycle
```

The required `DATABASE_GATEWAY_CONFIG_FILE` is an object keyed by destination
name. Each value selects its `backend`, Tailscale `capability`, tailnet ports,
upstream, and credential mapping; multiple instances of either database type
can run in the same process and image.

Each backend consumes the same `credentials.Provider` contract. Adding a
database means implementing its wire/authentication adapter and `backend.Backend`
while reusing the existing identity and credential lifecycle.

Vault-backed dynamic credentials create a temporary database login on demand. The file provider remains
available for development and external secret management, but does not itself
mint identity-specific database users.

Vault Proxy owns the dynamic-token and lease cache, renewal, and expiry
lifecycle. The gateway only distinguishes connection-scoped native credentials
from reusable HTTP credentials and explicitly revokes rejected or disconnected
connection-scoped tokens through the Proxy.

See [ClickHouse](docs/clickhouse.md) and [PostgreSQL](docs/postgres.md) for
protocol behavior and configuration.

Run the complete module suite with:

```console
go test -race ./...
```

## Build and run

Use Go 1.26.7 or newer:

```sh
go build -o database-gateway ./cmd/database-gateway
export DATABASE_GATEWAY_CONFIG_FILE="$PWD/examples/config.json"
export TS_HOSTNAME=database-gateway
export TS_STATE_DIR="$PWD/state"
export TS_AUTHKEY_FILE=/path/to/tailscale-auth-key
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN_ROLE=database-gateway-database
./database-gateway
```

The example requires a configured Vault Proxy and database secrets engines.
See [configuration and Vault setup](docs/setup.md) before running it. For local
experiments, use `credential_files` with existing database credentials instead.
Database clients select a granted role as their username; the gateway obtains
the real database credentials and authenticates upstream on their behalf.

With Nix, `nix build` builds the executable and `nix develop` opens a development
shell. The development shell includes PostgreSQL and Python, plus ClickHouse
on Linux. Integration tests start temporary databases and clean them up:

```sh
nix develop -c ./scripts/test-postgres.sh
nix develop -c ./scripts/test-clickhouse.sh
```

The scripts also work without Nix when Go, Python 3, and the relevant database
binaries are on `PATH`. PostgreSQL integration tests must run as a non-root user.

## Container image

CI builds and smoke-tests the Linux amd64 image on pull requests. After tests
pass on `main`, it publishes to `ghcr.io/dialohq/tailscale-database-gateway`
with `sha-<full-git-commit>` and `latest` tags. Pull requests do not publish images.
Pin a digest in deployments; `latest` is intended for trying the gateway.

The image runs as UID/GID `1000:1000` and includes CA certificates. Mount the
gateway configuration, auth key, and any credential files read-only. Provide a
writable state directory owned by UID 1000 and set `TS_STATE_DIR` to its mount
path. Configure Vault Proxy and upstream connectivity as described in
[setup](docs/setup.md); the image does not include a database or Vault Proxy.

To build and load the same image locally on Linux:

```sh
nix build .#dockerImage
docker load --input result
docker image inspect ghcr.io/dialohq/tailscale-database-gateway:local
```

The Nix image reuses the standalone Go package and its tests. It is also part
of `nix flake check` on Linux.
