FROM golang:1.24-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Defaults to a dev build (update checks disabled; see version.IsValid) so a
# manual `docker build` without --build-arg never misreports its version.
# CI always passes --build-arg APP_VERSION=<tag> for real release images.
ARG APP_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -tags headless \
    -trimpath \
    -ldflags="-s -w -X foxtrack-bridge/version.AppVersion=${APP_VERSION} -X foxtrack-bridge/version.AppBuildVariant=headless" \
    -o /out/foxtrack-bridge .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S foxtrack \
    && adduser -S -D -H -G foxtrack foxtrack \
    && mkdir -p /data \
    && chown -R foxtrack:foxtrack /data

COPY --from=build /out/foxtrack-bridge /usr/local/bin/foxtrack-bridge

ENV HOME=/data \
    XDG_CONFIG_HOME=/data

USER foxtrack
WORKDIR /data

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/version >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/foxtrack-bridge"]
