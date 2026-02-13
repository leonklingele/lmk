{
  inputs = {
    flake-utils.url = "github:numtide/flake-utils";
    nixpkgs-unstable.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs =
    {
      self,
      nixpkgs-unstable,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs-unstable { inherit system; };
      in
      {
        formatter = pkgs.nixfmt-tree;

        devShells.default = pkgs.mkShell {
          nativeBuildInputs = [
            pkgs.gnumake

            # Backend
            pkgs.go_1_26
            pkgs.gopls
            pkgs.golangci-lint
            pkgs.air
          ];

          shellHook = ''
            export CGO_ENABLED="0"
            export GOPATH="$(realpath "$PWD")/.gopath"
            export GOBIN="$GOPATH/bin"
            export GOCACHE="$GOPATH/.cache"
            export PATH="$PATH:$GOBIN"
          '';
        };
      }
    );
}
