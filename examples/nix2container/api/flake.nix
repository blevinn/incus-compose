{
  description = "Custom microservice image built with nix2container";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nix2container.url = "github:nlewo/nix2container";
    nix2container.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      nix2container,
    }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          n2c = nix2container.packages.${system}.nix2container;

          apiBin = pkgs.buildGoModule {
            pname = "api";
            version = "0.1.0";
            src = ./.;
            vendorHash = null;
            env.CGO_ENABLED = "0";
          };
        in
        {
          api = n2c.buildImage {
            name = "nix2builder-api";
            config = {
              Entrypoint = [ "${apiBin}/bin/nix2builder-api" ];
              ExposedPorts = {
                "8000/tcp" = { };
              };
              Healthcheck = {
                Test = [
                  "CMD"
                  "${apiBin}/bin/nix2builder-api"
                  "-healthcheck"
                ];
                Interval = 5000000000;
                Timeout = 2000000000;
                Retries = 3;
              };
              Env = [
                "PORT=8000"
                "CACHE_HOST=cache"
                "CACHE_PORT=6379"
              ];
            };
            layers = [
              (n2c.buildLayer {
                deps = [ apiBin ];
              })
            ];
          };
        }
      );
    };
}
