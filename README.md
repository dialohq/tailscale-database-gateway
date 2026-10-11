# Tailscale database gateway

Tailscale database gateway is a PostgreSQL and ClickHouse proxy that authorizes
database users based on Tailscale ACLs. Clients connect with their existing database
tools, using a granted role as their username instead of a database password. The gateway
acts as a proxy and authenticates the user to an upstream database using
either a static user/pass from a file or dynamic identities thru Hasicorp Vault.

It supports PostgreSQL, ClickHouse native TCP, and ClickHouse HTTP in one process.

## Usage

Use the published container image with Docker, Kubernetes, or another OCI
runtime.

```sh
ghcr.io/dialohq/tailscale-database-gateway
```

### Configuration

First provision a tailnet auth key, network access and application grants, and
database roles. For dynamic credentials, also configure Vault's database secrets
engine and an authenticated Vault Proxy as described in [setup](docs/setup.md).
The gateway does not provision these resources or define your SQL permissions.

Create a local `gateway/` directory containing:

- `config.json`: adapt [the example configuration](examples/config.json) to your
  upstream hosts, capabilities, and Vault role mappings. Remove unused destinations.
- `authkey`: your Tailscale auth key, readable only by the account running the gateway.
- Any database CA files referenced by your config, using paths under
  `/etc/database-gateway/` inside the container.

Create `gateway.env` with these container-side paths and your Vault Proxy address:

```dotenv
DATABASE_GATEWAY_CONFIG_FILE=/etc/database-gateway/config.json
TS_HOSTNAME=database-gateway
TS_STATE_DIR=/var/lib/database-gateway
TS_AUTHKEY_FILE=/etc/database-gateway/authkey
VAULT_ADDR=https://vault-proxy.example.com:8200
VAULT_TOKEN_ROLE=database-gateway-database
```

Keep Vault Proxy private and restrict access to the gateway. A remote Proxy must
use HTTPS with a certificate trusted by the container. If it runs in the same
network namespace (for example, a Kubernetes sidecar), use its loopback address,
such as `http://127.0.0.1:8200`. Another Docker container is not on loopback unless
you explicitly share its network namespace.

For development or externally managed credentials, use `credential_files`
instead of `vault_roles` as described in [setup](docs/setup.md#file-credentials).
Mount those files under the same configuration directory and omit the Vault
environment variables. This uses existing logins rather than creating users.

### Run with Docker

On Linux, from the directory containing `gateway/` and `gateway.env`:

```sh
mkdir -p state
docker run -d --name database-gateway \
  --user "$(id -u):$(id -g)" \
  --env-file gateway.env \
  --mount type=bind,src="$PWD/gateway",dst=/etc/database-gateway,readonly \
  --mount type=bind,src="$PWD/state",dst=/var/lib/database-gateway \
  --tmpfs /tmp:mode=1777 \
  "$IMAGE"
docker logs -f database-gateway
```

The image defaults to UID/GID `1000:1000`; this command overrides it to match the
host user who owns `state/` and the mounted files. The upstream hosts in your
configuration must be reachable from the container's network.

<details>
<summary>Run with Kubernetes instead</summary>

In your chosen namespace, create resources from the same files. The Secret
contains your auth key; do not commit its contents to source control.

```sh
kubectl create secret generic database-gateway-files --from-file=gateway/
kubectl create configmap database-gateway-env --from-env-file=gateway.env
```

Save this as `database-gateway.yaml`, then run
`kubectl apply -f database-gateway.yaml` in the same namespace:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: database-gateway
spec:
  replicas: 1
  selector:
    matchLabels:
      app: database-gateway
  template:
    metadata:
      labels:
        app: database-gateway
    spec:
      nodeSelector:
        kubernetes.io/os: linux
        kubernetes.io/arch: amd64
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: gateway
          image: ghcr.io/dialohq/tailscale-database-gateway@sha256:e074491f9880c729bc36bb2ab08988afdeec0cedf0c1422af0b5005b43bc63e2
          envFrom:
            - configMapRef:
                name: database-gateway-env
          env:
            - name: TS_HOSTNAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - {name: config, mountPath: /etc/database-gateway, readOnly: true}
            - {name: state, mountPath: /var/lib/database-gateway}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: config
          secret:
            secretName: database-gateway-files
            defaultMode: 0440
        - name: state
          emptyDir: {}
        - name: tmp
          emptyDir: {}
```

This example assumes an existing remote Vault Proxy; it does not deploy Vault.
State survives container restarts but not pod replacement. Use a reusable,
pre-authorized Tailscale auth key for replacement pods. Each pod gets its own
hostname; use Service mode below for a stable client endpoint and multiple replicas.

</details>

### Connect

From an authorized tailnet client, substitute your gateway's tailnet DNS name
and database name (or the pod's tailnet name for the Kubernetes example):

```sh
psql 'host=database-gateway.example.ts.net port=5432 dbname=app user=readonly'
clickhouse-client --host database-gateway.example.ts.net --user readonly
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
longer needed. For Kubernetes, delete the Deployment, ConfigMap, and Secret you
created. Remove any dedicated auth keys, grants, Service configuration, and Vault
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
