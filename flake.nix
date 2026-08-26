{
  description = "Wings";

  inputs = {
    flake-parts.url = "github:hercules-ci/flake-parts";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = {...} @ inputs:
    inputs.flake-parts.lib.mkFlake {inherit inputs;} {
      systems = inputs.nixpkgs.lib.systems.flakeExposed;

      imports = [
        inputs.treefmt-nix.flakeModule
      ];

      perSystem = {system, ...}: let
        pkgs = import inputs.nixpkgs {inherit system;};
        secureGo = pkgs.go_1_26.overrideAttrs (_: {
          version = "1.26.7";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go1.26.7.src.tar.gz";
            hash = "sha256-DtJOrHVRBQhbif6cq8J0K5GgrXuUtZ0602SRjryJVq0=";
          };
        });
      in {
         devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            secureGo
            gofumpt
            golangci-lint
            gotools
          ];
        };

        treefmt = {
          projectRootFile = "flake.nix";

          programs = {
            alejandra.enable = true;
            deadnix.enable = true;
            gofumpt = {
              enable = true;
              extra = true;
            };
            shellcheck.enable = true;
            shfmt = {
              enable = true;
              indent_size = 0; # 0 causes shfmt to use tabs
            };
            yamlfmt.enable = true;
          };
        };
      };
    };
}
