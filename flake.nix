{
  description = "Tailscale identity-aware PostgreSQL and ClickHouse gateway";
  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["x86_64-linux" "aarch64-linux" "aarch64-darwin"];
    eachSystem = nixpkgs.lib.genAttrs systems;
  in {
    packages = eachSystem (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      default = import ./nix/package.nix {inherit pkgs;};
    });
    checks = eachSystem (system: {build = self.packages.${system}.default;});
    devShells = eachSystem (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      default = pkgs.mkShell {
        packages =
          [pkgs.go pkgs.gopls pkgs.python3 pkgs.postgresql_17]
          ++ pkgs.lib.optionals pkgs.stdenv.isLinux [pkgs.clickhouse];
      };
    });
  };
}
