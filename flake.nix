{
  description = "Nebula Nomad CNI plugin and agent";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs =
    inputs@{ self, ... }:
    inputs.flake-parts.lib.mkFlake { inherit inputs; }

      {

        systems = [
          "x86_64-linux"
          "aarch64-linux"
          "aarch64-darwin"
        ];

        perSystem =
          {
            config,
            pkgs,
            ...
          }:
          let
            # Use git describe for version, fall back to rev or dirty
            version = if (self ? rev) then self.shortRev else "dirty";
            # Full commit hash
            gitCommit = if (self ? rev) then self.rev else "unknown";
          in
          {
            packages = {
              nebula-nomad-cni = pkgs.buildGoModule {
                pname = "nebula-nomad-cni";
                inherit version;
                src = ./.;

                vendorHash = "sha256-arE22XxWwmkzFx0+C5j2y0+qJfet038XFy4efic/zXc=";

                # Build both binaries
                subPackages = [
                  "cmd/nebula-nomad-cni"
                  "cmd/nebula-nomad-agent"
                ];

                ldflags = [
                  "-s"
                  "-w"
                  "-X github.com/adriansalamon/nebula-nomad-cni/pkg/version.Version=${version}"
                  "-X github.com/adriansalamon/nebula-nomad-cni/pkg/version.GitCommit=${gitCommit}"
                ];

                meta = with pkgs.lib; {
                  description = "Nebula CNI plugin for Nomad with agent support";
                  homepage = "https://github.com/adriansalamon/nebula-nomad-cni";
                  license = licenses.mit; # Update if different
                  maintainers = [ ];
                };
              };

              default = config.packages.nebula-nomad-cni;
            };

            # Development shell with Go tooling
            devShells.default = pkgs.mkShell {
              buildInputs = with pkgs; [
                go
                gopls
                gotools
                go-tools
              ];

              shellHook = ''
                echo "Nebula Nomad CNI development environment"
                echo "Go version: $(go version)"
              '';
            };
          };

      };
}
