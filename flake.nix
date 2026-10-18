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
      };
    };
}
