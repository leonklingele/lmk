with import <nixpkgs> {}; let
  unstable = pkgs.unstable or (import <unstable> {});
in mkShell {
  name = "lmk";

  nativeBuildInputs = [
    gnumake

    go
    unstable.golangci-lint

    air
  ];

  shellHook = ''
    rootdir="$(realpath "$PWD")"
    if [[ -n "$IN_LORRI_SHELL" ]]; then
      rootdir="$(dirname "$IN_LORRI_SHELL")"
    fi
    export GOPATH="$rootdir/.gopath"
    export GOBIN="$GOPATH/bin"
    export GOCACHE="$GOPATH/.cache"
    export PATH="$PATH:$GOBIN"
  '';
}
