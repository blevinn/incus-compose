# nix2container Example

Demonstrates building OCI container images with `nix2container` directly inside
`incus-compose`, combining:

1. **Upstream Nix flake images** (`caddy`, `valkey`) evaluated and built
   directly from nixpkgs/flake inputs.
2. **A custom in-tree Nix image** (`api`) defined in `api/flake.nix` and Go
   microservice built hermetically with Nix.

## Project Structure

```
examples/nix2container/
├── compose.yaml          # Multi-service composition with volumes and configs
├── caddy/
│   └── Caddyfile         # Reverse proxy configuration
├── api/                  # Custom Nix-built service
│   ├── flake.nix         # nix2container image recipe
│   ├── go.mod
│   └── main.go           # Go HTTP microservice with Valkey ping
└── README.md
```

## Architecture

- **`proxy`** (`caddy`): Reverse proxy on port 8080 routing to the API backend,
  with `caddy_config` volume mounted at `/etc/caddy`.
- **`api`** (custom Go service): Built via `nix2container` from `./api`
  (`.#api`). Connects to `valkey` to ping cache status and serves JSON
  responses.
- **`cache`** (`valkey`): Redis-compatible in-memory cache built via
  `nix2container` from `github:nix-containers/images#valkey`, with persistent
  `/data` volume.

## Usage

```bash
# Start the stack (evaluates and builds the Nix container images into Incus)
incus-compose up -d

# Test the API through the reverse proxy
curl http://localhost:8080/

# Tear down the stack
incus-compose down
```
