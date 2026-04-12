{
  description = "Nebula Nomad CNI plugin and agent";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs =
    inputs@{ self, ... }:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      imports = [
        inputs.flake-parts.flakeModules.easyOverlay
      ];

      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];

      flake =
        { config, ... }:
        {
          nixosModules.default = {
            nixpkgs.overlays = [ config.overlays.default ];
          };
        };

      perSystem =
        {
          config,
          pkgs,
          ...
        }:
        let
          # TODO: get this some better way, for now just hard code
          baseVersion = "0.1.0";

          # Version format: vx.x.x-sha
          gitShortRev = if (self ? rev) then self.shortRev else "dirty";
          version = "${baseVersion}-${gitShortRev}";
          gitCommit = if (self ? rev) then self.rev else "unknown";
        in
        {
          overlayAttrs = {
            inherit (config.packages) nebula-nomad-cni;
          };

          packages = {
            nebula-nomad-cni = pkgs.buildGoModule {
              pname = "nebula-nomad-cni";
              inherit version;
              src = ./.;

              vendorHash = "sha256-HISga5VfYnHn/q+x6njSgtnzBxnS1RTlscwkTIaZGOk=";

              # Build both binaries
              subPackages = [
                "cmd/nebula-nomad-cni"
                "cmd/nebula-nomad-agent"
                "cmd/nebula-nomad-worker"
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
