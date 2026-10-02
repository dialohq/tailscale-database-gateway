{
  pkgs,
  gateway,
}:
pkgs.dockerTools.buildLayeredImage {
  name = "ghcr.io/dialohq/tailscale-database-gateway";
  tag = "local";
  contents = [gateway pkgs.dockerTools.caCertificates];
  config = {
    Entrypoint = ["/bin/database-gateway"];
    Env = ["SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"];
    User = "1000:1000";
    Labels."org.opencontainers.image.source" = "https://github.com/dialohq/tailscale-database-gateway";
  };
}
