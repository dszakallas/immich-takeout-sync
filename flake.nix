{
  description = "Google Photos Takeout to Immich synchronization service";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        immich-takeout-sync = pkgs.buildGoModule rec {
          pname = "immich-takeout-sync";
          version = "0.1.0";
          src = pkgs.lib.cleanSource ./.;

          vendorHash = "sha256-Spo2ckfftabsomFOVNSKb8Q8XYETtEMDJ+0Q8RIp2Ao=";

          ldflags = [
            "-s"
            "-w"
            "-X github.com/dszakallas/immich-takeout-sync/cmd.Version=${version}"
          ];

          env.CGO_ENABLED = 0;
          subPackages = [ "." ];
        };

        dockerImage = pkgs.dockerTools.buildLayeredImage {
          name = "immich-takeout-sync";
          tag = "latest";
          contents = [
            immich-takeout-sync
            pkgs.immich-go
            pkgs.cacert
            pkgs.tzdata
          ];
          config = {
            Entrypoint = [ "${immich-takeout-sync}/bin/immich-takeout-sync" ];
            Cmd = [ "sync" ];
            Env = [
              "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
              "IMMICH_GO_BIN=${pkgs.immich-go}/bin/immich-go"
            ];
            Volumes = {
              "/data" = { };
              "/scratch" = { };
            };
          };
        };
      in
      {
        packages = {
          default = immich-takeout-sync;
          immich-takeout-sync = immich-takeout-sync;
          takeout-sync = immich-takeout-sync;
          inherit dockerImage;
        };

        dockerImages = {
          immich-takeout-sync = dockerImage;
          takeout-sync = dockerImage;
          default = dockerImage;
        };
      }
    );
}
