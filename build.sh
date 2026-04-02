#!/usr/bin/env bash
set -e
APP="foxtrack-bridge"
OUT="./dist"
mkdir -p "$OUT"

echo "Downloading dependencies..."
go mod tidy

echo "Building for all platforms..."
GOOS=darwin  GOARCH=arm64  go build -ldflags="-s -w" -o "$OUT/${APP}-mac-arm64"         .
GOOS=darwin  GOARCH=amd64  go build -ldflags="-s -w" -o "$OUT/${APP}-mac-intel"         .
GOOS=linux   GOARCH=amd64  go build -ldflags="-s -w" -o "$OUT/${APP}-linux-amd64"       .
GOOS=linux   GOARCH=arm64  go build -ldflags="-s -w" -o "$OUT/${APP}-linux-arm64"       .
GOOS=windows GOARCH=amd64  go build -ldflags="-s -w -H=windowsgui" -o "$OUT/${APP}-windows-amd64.exe" .

echo ""
echo "Build complete. Binaries in $OUT/"
ls -lh "$OUT/"
