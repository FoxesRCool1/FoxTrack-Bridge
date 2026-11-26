# Docker

FoxTrack Bridge can run in a container using the headless Linux build. The container stores settings and print history under `/data`.

## Build locally

From the repository root:

```bash
docker build \
  --build-arg APP_VERSION=v2.6.6 \
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

`--network host` lets the Bridge find Bambu printers: their SSDP broadcasts do not cross Docker's default bridge network. It makes `-p` unnecessary; change the port with `-e FOXTRACK_BRIDGE_PORT=9000`. Without it (for example `-p 8080:8080`) the dashboard works, but printers must be added by address. The repository's `docker-compose.yml` sets this up for you. On Docker Desktop (Windows, macOS), turn on host networking in its settings, or the dashboard will not open.

## Behind a reverse proxy

To open the dashboard on your own domain through a reverse proxy (nginx, Caddy, Traefik and the like), the Bridge needs a few things:

- **Allow the domain.** The Bridge answers only to names it knows, so a website cannot pretend to be it. Set `FOXTRACK_BRIDGE_ALLOWED_HOSTS` to the public host name, for example `bridge.example.com` (several names are separated by commas), and restart the Bridge. In `docker-compose.yml` this is the commented line under `environment:`; with `docker run`, add `-e FOXTRACK_BRIDGE_ALLOWED_HOSTS=bridge.example.com`. The Bridge reads the name from the request's `Host` header, so the proxy must pass it on as it received it. Until then, that name gets a "forbidden" page (or a JSON error from scripts). IP addresses, `localhost`, names with no dot and names ending in `.local`, `.lan`, `.home.arpa`, `.internal` or `.ts.net` (Tailscale) work without it.
- **Send `X-Forwarded-Proto`.** The Bridge itself speaks plain HTTP, so it marks the session cookie it sets at sign-in `Secure` only when the proxy sends `X-Forwarded-Proto: https`. Without that header the cookie is not marked `Secure`.
- **Use the root of a domain.** A sub-path such as `https://example.com/bridge/` does not work: the dashboard asks for `/api/...` and its other files by absolute path, so they would not go through the proxy.
- **Set a password first.** Without one, anyone who can reach the proxy can open the dashboard and control your printers. A request that comes with `X-Forwarded-For`, `X-Real-Ip` or `Forwarded` is never treated as coming from the Bridge's own computer, so while no password is set, Back up and restore refuse it. Set the password under **Settings > Security**.

## Build from a fork or branch

Docker can build directly from a GitHub repository:

```bash
docker build \
  --build-arg APP_VERSION=v2.6.6 \
  -t foxtrack-bridge \
  https://github.com/FoxesRCool1/FoxTrack-Bridge.git#main
```

Replace the repository, branch, and `APP_VERSION` value for your own fork or release.

## Published images

The Docker workflow builds images for pull requests and publishes multi-platform `linux/amd64` and `linux/arm64` images to GitHub Container Registry on pushes to `main`, on `v*` tags, and on manual workflow runs. Only a `v*` release tag moves `latest` (and the version tags); a push to `main` publishes the `main` tag, for trying unreleased changes.

Images are published as:

```text
ghcr.io/<owner>/foxtrack-bridge
```

This repository publishes:

```bash
docker pull ghcr.io/foxesrcool1/foxtrack-bridge:latest
```

Run a published image with:

```bash
docker run -d \
  --name foxtrack-bridge \
  --network host \
  -v foxtrack-bridge-data:/data \
  --restart unless-stopped \
  ghcr.io/foxesrcool1/foxtrack-bridge:latest
```

The first package published from a fork may be private by default. If you want a server to pull it without authenticating, make the package public in the repository package settings. Otherwise, run `docker login ghcr.io` on the server before pulling.

## Cameras and timelapse

The image includes `ffmpeg`, which the Bridge uses for two things:

- **Bambu X1, H2 and P2S cameras.** These printers stream their camera over RTSP on port 322, which the Bridge reads with ffmpeg. A1 and P1 printers use a different stream and do not need it. Make sure Liveview is switched on in the printer's settings, and that the container can reach the printer on port 322 (`--network host` does).
- **Timelapse.** With ffmpeg the Bridge builds an MP4 video from the pictures it takes while a printer prints. Without it, it saves an animated GIF instead, which is bigger and lower quality.

Timelapse pictures and videos are stored in the Bridge's data folder, so they sit in `/data` and survive container updates. The pictures are deleted once the video is built, and videos older than 30 days are deleted automatically (change this under Settings > Timelapse).

If you run the Bridge outside Docker, install ffmpeg yourself (for example `apt install ffmpeg`, `brew install ffmpeg` or `winget install ffmpeg`) and restart the Bridge. To use an ffmpeg that is not on the `PATH`, set `FOXTRACK_BRIDGE_FFMPEG` to its full path.

## Notes

- The image runs as a non-root `foxtrack` user.
- Persist `/data` to keep settings and print history across container updates.
- The app listens on port `8080` inside the container.
- In container deployments, update by rebuilding or pulling a new image and recreating the container. The in-app updater is intended for native binary installs.
