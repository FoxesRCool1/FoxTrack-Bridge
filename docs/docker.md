# Docker

FoxTrack Bridge can run in a container using the headless Linux build. The container stores settings and print history under `/data`.

## Build locally

From the repository root:

```bash
docker build \
  --build-arg APP_VERSION=v2.2.0 \
  -t foxtrack-bridge .
```

Use the latest FoxTrack Bridge release tag for `APP_VERSION` when building images for regular server use. The update checker compares this value with the latest GitHub release; branch names, commit hashes, or other non-release values may be reported as older than the current release.

For development builds, pass a unique version string if you want the dashboard to show exactly which image is running:

```bash
docker build \
  --build-arg APP_VERSION="$(git describe --tags --always --dirty)" \
  -t foxtrack-bridge .
```

## Run

```bash
docker run -d \
  --name foxtrack-bridge \
  --network host \
  -v foxtrack-bridge-data:/data \
  --restart unless-stopped \
  foxtrack-bridge
```

Open the dashboard at [http://localhost:8080](http://localhost:8080).

`--network host` lets the Bridge find Bambu printers: their SSDP broadcasts do not cross Docker's default bridge network. It is Linux-only and makes `-p` unnecessary; change the port with `-e FOXTRACK_BRIDGE_PORT=9000`. Without it (for example `-p 8080:8080`) the dashboard works, but printers must be added by address. The repository's `docker-compose.yml` sets this up for you.

## Build from a fork or branch

Docker can build directly from a GitHub repository:

```bash
docker build \
  --build-arg APP_VERSION=v2.2.0 \
  -t foxtrack-bridge \
  https://github.com/jonathanq/FoxTrack-Bridge.git#add-docker-support
```

Replace the repository, branch, and `APP_VERSION` value for your own fork or release.

## Published images

The Docker workflow builds images for pull requests and publishes multi-platform `linux/amd64` and `linux/arm64` images to GitHub Container Registry on pushes to `main`, on `v*` tags, and on manual workflow runs.

For this repository, images are published as:

```text
ghcr.io/<owner>/foxtrack-bridge
```

For example, a fork owned by `jonathanq` publishes:

```bash
docker pull ghcr.io/jonathanq/foxtrack-bridge:latest
```

Run a published image with:

```bash
docker run -d \
  --name foxtrack-bridge \
  --network host \
  -v foxtrack-bridge-data:/data \
  --restart unless-stopped \
  ghcr.io/jonathanq/foxtrack-bridge:latest
```

The first package published from a fork may be private by default. If you want a server to pull it without authenticating, make the package public in the repository package settings. Otherwise, run `docker login ghcr.io` on the server before pulling.

## Notes

- The image runs as a non-root `foxtrack` user.
- Persist `/data` to keep settings and print history across container updates.
- The app listens on port `8080` inside the container.
- In container deployments, update by rebuilding or pulling a new image and recreating the container. The in-app updater is intended for native binary installs.
