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

```text
ghcr.io/dialohq/tailscale-database-gateway:<tag>
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

Both a config file and environment variables are required.

#### Config file

Make this JSON file available to the container, for example at
`/etc/database-gateway/config.json`. Replace the database hostname and port with
your own. The `readonly` role maps to a database credentials file:

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
mount it at `/etc/database-gateway/ca.crt`, and replace `sslrootcert=system` with
`sslrootcert=/etc/database-gateway/ca.crt`.

Provide the database login at `/etc/database-gateway/readonly.json`:

```json
{"username": "existing_database_user", "password": "database_password"}
```

Keep the credentials file private and out of source control. `readonly` is the
role clients request, not a permission the gateway creates: give the upstream
login the appropriate SQL privileges yourself.

[In your tailnet policy](https://login.tailscale.com/admin/acls), allow clients to reach the gateway on port 5432 and grant
them `example.com/cap/postgres` with `[{"role":"readonly"}]`. See
[setup](docs/setup.md#tailscale) for application grant syntax.

#### Environment variables

Set these in your container runtime alongside the config file. All paths refer
to locations **inside the container**:

| Variable | Required? | Purpose / example |
| --- | --- | --- |
| `DATABASE_GATEWAY_CONFIG_FILE` | **Required** | Path to the JSON config, e.g. `/etc/database-gateway/config.json`. |
| `TS_HOSTNAME` | **Required** | The gateway's name on your tailnet, e.g. `database-gateway`. |
| `TS_STATE_DIR` | **Required** | Writable directory for Tailscale state, e.g. `/var/lib/database-gateway`. |
| `TS_AUTHKEY_FILE` | **Optional** | Mounted secret containing the Tailscale auth key, e.g. `/etc/database-gateway/authkey`. Preferred over environment-based secrets. |
| `TS_AUTHKEY` | **Optional** | Tailscale auth key supplied directly; used when `TS_AUTHKEY_FILE` is unset. `TS_AUTH_KEY` is a fallback alias. |
| `TS_CLIENT_SECRET` | **Optional** | Tailscale OAuth client secret for enrollment; used when no auth key is supplied. Requires advertised tags. |
| `TS_SERVICE_NAME` | **Optional** | Tailscale Service to advertise, e.g. `svc:database-gateway`. Create the Service and approve its advertisements first. Unset: listen on the gateway's own tailnet node. |
| `TS_ADVERTISE_TAGS` | **Optional**; required for Service mode or OAuth enrollment | Comma-separated tags, e.g. `tag:database-gateway`. |
| `TS_CONTROL_URL` | **Optional** | Custom control server URL, e.g. for Headscale. Unset: use Tailscale's control server. |
| `VAULT_ADDR` | **Optional**; required when using `vault_roles` | Authenticated Vault Proxy's listener URL. HTTP is allowed only on loopback; remote addresses require HTTPS. |
| `VAULT_TOKEN_ROLE` | **Optional**; required when using `vault_roles` | Vault child-token role, e.g. `database-gateway-database`. |
| `SSL_CERT_FILE` | **Optional** | Override the system CA bundle. The image sets `/etc/ssl/certs/ca-bundle.crt`. |

For unattended enrollment of a new node, supply an auth-key file, an auth-key
environment variable, or an OAuth client secret—not all three. `VAULT_TOKEN` and
`VAULT_NAMESPACE` are deliberately ignored; Vault Proxy handles authentication.

Mount configuration and secret files read-only. The image runs as UID/GID
`1000:1000`; that user must be able to read the files and write to the state
directory and `/tmp`. Database hosts must be reachable from the container.
Client connections arrive over Tailscale, so no host port publishing is needed.
Logs go to standard output.

<details>
<summary>Kubernetes</summary>

Use the same image, environment variables, and configuration paths in a
Deployment. Mount the auth key and database login as files from Kubernetes
Secrets, mount the configuration read-only, and provide writable state and `/tmp` volumes.
Set `runAsUser`, `runAsGroup`, and `fsGroup` to `1000` for the image's default user.
A Kubernetes Service is not required for tailnet access.

For replaceable pods, use a reusable, pre-authorized auth key and a unique
`TS_HOSTNAME` per pod. Use `TS_SERVICE_NAME` for a stable client endpoint.

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
