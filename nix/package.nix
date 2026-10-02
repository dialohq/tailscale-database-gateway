{pkgs}: let
  go = import ./go.nix {inherit pkgs;};
in
  (pkgs.buildGoModule.override {inherit go;}) {
    pname = "tailscale-database-gateway";
    version = "0.1.0";
    src = with pkgs.lib.fileset;
      toSource {
        root = ../.;
        fileset = unions [
          ../go.mod
          ../go.sum
          ../cmd
          ../internal
        ];
      };
    vendorHash = "sha256-fWCgiN1PBMwvFdmlclIjDx5FUYJH4p3MJYV0zXpoi+E=";
    subPackages = ["cmd/database-gateway"];
    env.CGO_ENABLED = "0";
    ldflags = ["-s" "-w"];
    doCheck = true;
  }
