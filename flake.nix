{
  description = "Dev environment for tally";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
    in
    {
      devShells.${system}.default = pkgs.mkShell {
        # The SQLite driver is pure Go, so the Dockerfile builds with CGO_ENABLED=0. cgo stays
        # on here because go test -race needs it.
        packages = with pkgs; [
          go
          gopls
          sqlite
        ];

        # Keep Go's module and build caches in the checkout rather than the home directory.
        # Worktrees share the main checkout's caches. -modcacherw keeps them deletable with rm -rf.
        shellHook = ''
          root=$(dirname "$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || echo "$PWD/.git")")
          export GOMODCACHE="$root/.cache/go-mod"
          export GOCACHE="$root/.cache/go-build"
          export GOFLAGS=-modcacherw
          unset root
        '';
      };
    };
}
