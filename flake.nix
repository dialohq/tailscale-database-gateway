{
  description = "Tailscale identity-aware PostgreSQL and ClickHouse gateway";
  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  inputs.nix2container = {
    url = "github:dialohq/nix2container/c1a53d1a50ab47a7acd7856a79809b8fdaf083a1";
    inputs.nixpkgs.follows = "nixpkgs";
  };
  outputs = {
    self,
    nixpkgs,
    nix2container,
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
        image = import ./nix/image.nix {
          inherit pkgs gateway;
          inherit (nix2container.packages.${system}) nix2container;
        };
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
