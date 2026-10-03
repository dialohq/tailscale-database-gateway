{
  pkgs,
  gateway,
  nix2container,
}:
nix2container.buildImage {
  name = "ghcr.io/dialohq/tailscale-database-gateway";
  config = {
    entrypoint = ["/bin/database-gateway"];
    env = ["SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"];
    user = "1000:1000";
    labels."org.opencontainers.image.source" = "https://github.com/dialohq/tailscale-database-gateway";
  };
  copyToRoot = pkgs.buildEnv {
    name = "database-gateway";
    paths = [gateway pkgs.dockerTools.caCertificates];
    pathsToLink = ["/bin" "/etc"];
  };
}
