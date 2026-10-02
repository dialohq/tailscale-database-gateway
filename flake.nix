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
      images = import ./nix/images.nix {
        inherit pkgs gateway;
        inherit (nix2container.packages.${system}) nix2container;
      };
    in
      {default = gateway;}
      // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
        dockerImage = images.database-gateway;
        pushImages = import ./nix/push-images.nix {inherit pkgs images;};
      });
    checks = eachSystem (system:
      {build = self.packages.${system}.default;}
      // nixpkgs.lib.optionalAttrs nixpkgs.legacyPackages.${system}.stdenv.isLinux {
        pushImages = self.packages.${system}.pushImages;
      });
    apps = eachSystem (system:
      nixpkgs.lib.optionalAttrs nixpkgs.legacyPackages.${system}.stdenv.isLinux {
        pushImages = {
          type = "app";
          program = "${self.packages.${system}.pushImages}/bin/push-images";
        };
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
