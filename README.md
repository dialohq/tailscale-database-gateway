# Tailscale database gateway

Tailscale database gateway is a proxy that gives users passwordless database
access based on their Tailscale identity and application grants. It authenticates
to the upstream database using either a username and password from a file or
temporary credentials issued by HashiCorp Vault.

It supports PostgreSQL, ClickHouse native TCP, and ClickHouse HTTP in one process.

## Usage

Use the published container image with Docker, Kubernetes, or another OCI
runtime. Replace `<tag>` with a published tag from
[the container registry](https://github.com/dialohq/tailscale-database-gateway/pkgs/container/tailscale-database-gateway):

```sh
IMAGE='ghcr.io/dialohq/tailscale-database-gateway:<tag>'
```

### What you'll need

This example uses an existing PostgreSQL login, with no Vault setup:

- **A Tailscale auth key**, created in the Tailscale admin console, so the gateway
  can join your tailnet.
- **Your database's hostname, port, database name, username, and password**,
  from your database administrator or provider. The login must
  already have the SQL permissions you want clients to use.
- **Access to edit your tailnet policy**, or an administrator who can grant
  clients access to the gateway.

<details>
<summary>Temporary database credentials with HashiCorp Vault (advanced)</summary>

Instead of keeping a database login in a file, Vault can create temporary logins
for authorized callers. You'll need a configured database secrets engine,
dynamic roles and policies, and an authenticated Vault Proxy.

Use `vault_roles` instead of `credential_files` in the database configuration,
and set `VAULT_ADDR` to the authenticated Vault Proxy's listener URL and
`VAULT_TOKEN_ROLE` to your configured child-token role. Follow [Vault setup](docs/setup.md#vault); the
[combined example](examples/config.json) shows PostgreSQL and ClickHouse mappings.

</details>

### Configuration

Create `gateway/config.json` with your database's hostname and port. This maps
the client-visible role `readonly` to a database credentials file:

```json
{
  "postgres": {
    "backend": "postgres",
    "port": 5432,
    "upstream": "postgresql://postgres.example.com:5432/postgres?sslmode=verify-full&sslrootcert=system",
    "capability": "example.com/cap/postgres",
    "credential_files": {
      "readonly": "/etc/database-gateway/readonly.json"
    }
  }
}
```

This verifies the database certificate using the image's built-in CA bundle.
If your database uses a private CA, obtain its certificate from your administrator,
save it as `gateway/ca.crt`, and replace `sslrootcert=system` with
`sslrootcert=/etc/database-gateway/ca.crt`.

Put the database login in `gateway/readonly.json`:

```json
{"username": "existing_database_user", "password": "database_password"}
```

Keep the credentials file private and out of source control. `readonly` is the
role clients request, not a permission the gateway creates: give the upstream
login the appropriate SQL privileges yourself.

[In your tailnet policy](https://login.tailscale.com/admin/acls), allow clients to reach the gateway on port 5432 and grant
them `example.com/cap/postgres` with `[{"role":"readonly"}]`. See
[setup](docs/setup.md#tailscale) for application grant syntax.

Save the Tailscale auth key in `gateway/authkey`. Mount the configuration and
secret files read-only when starting the container:

```sh
chmod 600 gateway/authkey gateway/readonly.json
mkdir -p state
docker run -d --name database-gateway \
  --user "$(id -u):$(id -g)" \
  -e TS_AUTHKEY_FILE=/etc/database-gateway/authkey \
  -e TS_HOSTNAME=database-gateway \
  -e TS_STATE_DIR=/var/lib/database-gateway \
  -e DATABASE_GATEWAY_CONFIG_FILE=/etc/database-gateway/config.json \
  --mount type=bind,src="$PWD/gateway",dst=/etc/database-gateway,readonly \
  --mount type=bind,src="$PWD/state",dst=/var/lib/database-gateway \
  --tmpfs /tmp:mode=1777 \
  "$IMAGE"
docker logs -f database-gateway
```

The image defaults to UID/GID `1000:1000`; this command overrides it to match the
host user who owns `state/` and the mounted files. The upstream hosts in your
configuration must be reachable from the container's network. Client connections
arrive over Tailscale, so no Docker port publishing is needed. The gateway stores
Tailscale state in `state/` and writes logs to standard output.

<details>
<summary>Kubernetes</summary>

Use the same image, environment variables, and configuration paths in a
Deployment. Mount the auth key and database login as files from Kubernetes
Secrets, mount the configuration read-only, and provide writable state and `/tmp` volumes.
Set `runAsUser`, `runAsGroup`, and `fsGroup` to `1000` for the image's default user.
A Kubernetes Service is not required for tailnet access.

For replaceable pods, use a reusable, pre-authorized auth key and a unique
`TS_HOSTNAME` per pod. See Service mode below for a stable client endpoint.

</details>

### Connect

From an authorized tailnet client, substitute your gateway's tailnet DNS name
and database name:

```sh
psql 'host=database-gateway.example.ts.net port=5432 dbname=app user=readonly'
```

The PostgreSQL client must explicitly select both a role and a database. Its
connection to the gateway is encrypted by Tailscale rather than PostgreSQL TLS;
configure upstream TLS separately, using `sslmode=verify-full` and a trusted CA.

For multiple gateway instances behind a Tailscale Service, also set
`TS_SERVICE_NAME` and `TS_ADVERTISE_TAGS`, then create the Service and approve its
advertisements. Without `TS_SERVICE_NAME`, the gateway listens directly on its
own tailnet node.

### Stop and remove

For Docker, run `docker stop database-gateway` and `docker rm database-gateway`.
Remove the image with `docker image rm "$IMAGE"` and delete `state/` when no
longer needed. For Kubernetes, delete the resources you created.
Remove any dedicated auth keys, grants, Service configuration, and Vault
resources separately; stopping the gateway does not remove them.

## How it works

The gateway resolves the caller's Tailscale identity and requires an application
grant matching the destination's capability and requested role. It authenticates
to the upstream database on the caller's behalf, then forwards traffic. Database
roles remain responsible for SQL privileges.

<details>
<summary>Credential lifecycle and source layout</summary>

Vault-backed credentials create a temporary database login on demand. Vault Proxy
owns the token and lease cache, renewal, and expiry lifecycle. Native connections
use connection-scoped credentials; the gateway revokes their tokens on disconnect
or rejected authentication. HTTP requests can reuse credentials through the
Proxy's cache.

```text
cmd/database-gateway/     executable entrypoint
internal/
  backend/               contract implemented by each database backend
  database/              configures and serves the backends
  clickhouse/            native and HTTP protocol handling
  postgres/              startup/authentication and TCP forwarding
  credentials/           role selection, lifecycle, and file provider
    vault/               Vault Proxy tokens and dynamic credentials
  tailnet/               human and machine identity resolution
  config/                strict file and environment parsing
  service/               process lifecycle and connection serving
```

Each backend consumes the same `credentials.Provider` contract. Adding a database
means implementing its wire/authentication adapter and `backend.Backend`, while
reusing identity resolution and the credential lifecycle.

</details>

## Reference and tests

- [Setup](docs/setup.md): tailnet grants, Vault provisioning, and file credentials.
- [PostgreSQL](docs/postgres.md): configuration, TLS, startup parameters, and cancellation.
- [ClickHouse](docs/clickhouse.md): native and HTTP authentication, audit identity, and Service mode.

<details>
<summary>Build from source and run tests</summary>

With Git and Go 1.26.7 or newer:

```sh
git clone https://github.com/dialohq/tailscale-database-gateway.git
cd tailscale-database-gateway
go build -o database-gateway ./cmd/database-gateway
```

The build downloads dependencies into your Go cache and writes the executable
to the checkout. It does not install a service. Set the same environment
variables as above, using host paths instead of container paths, then run
`./database-gateway`. Stop with Ctrl-C or SIGTERM; delete the binary and its state
directory to remove a local installation.

With Nix flakes enabled, `nix build` builds `./result/bin/database-gateway`.
Nix downloads dependencies into its store. Remove the `result` link when finished;
Nix garbage collection can reclaim unreferenced store paths.

Run the unit suite, including the race detector, with:

```sh
go test -race ./...
```

`nix develop` provides Go, PostgreSQL, Python 3, and (on Linux) ClickHouse.
The integration scripts start temporary local databases, exercise the gateway,
and clean up afterward; they do not require a tailnet or Vault:

```sh
nix develop -c ./scripts/test-postgres.sh
nix develop -c ./scripts/test-clickhouse.sh
```

The scripts also work directly when their dependencies are on `PATH`.
PostgreSQL integration tests must run as a non-root user.

</details>

## License

[MIT](LICENSE). Copyright (c) 2026 Dialo contributors.
