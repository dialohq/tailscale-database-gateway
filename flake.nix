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
      gateway = import ./nix/package.nix {inherit pkgs;};
    in
      {default = gateway;}
      // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
        dockerImage = import ./nix/image.nix {inherit pkgs gateway;};
      });
    checks = eachSystem (system:
      {build = self.packages.${system}.default;}
      // nixpkgs.lib.optionalAttrs nixpkgs.legacyPackages.${system}.stdenv.isLinux {
        dockerImage = self.packages.${system}.dockerImage;
      });
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
