#!/usr/bin/env bash
# Build and publish a FoxTrack Bridge release.
#
#   scripts/release.sh v2.5.1            # test, build, then ask before merge + tag + push
#   scripts/release.sh v2.5.1 --build    # test and build only, no git changes
#
# Run it from the main checkout (not a worktree) with dev checked out and clean.
# GitHub Actions (.github/workflows/release.yml) builds every platform when the
# tag lands; this script only builds the Linux headless binary for the local
# release checklist in CLAUDE.md.
set -euo pipefail

tag="${1:-}"
mode="${2:-}"
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 vX.Y.Z [--build]" >&2
  exit 2
fi
version="${tag#v}"
cd "$(git rev-parse --show-toplevel)"

echo "== checks"
if [[ -n "$(git status --porcelain)" ]]; then
  echo "working tree is not clean; commit or stash first" >&2
  exit 1
fi
if [[ "$(git rev-parse --abbrev-ref HEAD)" != "dev" ]]; then
  echo "check out dev first" >&2
  exit 1
fi
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  echo "tag $tag already exists" >&2
  exit 1
fi
grep -q "^## $tag" CHANGELOG.md || { echo "CHANGELOG.md has no '## $tag' entry" >&2; exit 1; }

go vet ./...
go test -race ./...

echo "== build (Linux headless, version $version)"
mkdir -p dist
go build -tags headless \
  -ldflags="-s -w -X foxtrack-bridge/version.AppVersion=${version} -X foxtrack-bridge/version.AppBuildVariant=headless" \
  -o "dist/FoxTrack-Bridge-Linux-${version}" .
echo "built dist/FoxTrack-Bridge-Linux-${version}"

if [[ "$mode" == "--build" ]]; then
  exit 0
fi

cat <<MSG

Before you continue, run the release checklist in CLAUDE.md against
dist/FoxTrack-Bridge-Linux-${version} with your real config:
  - every printer shows on the dashboard on first load
  - a settings save keeps the printer list
  - /tailwind.css /icons.css /fonts.css /fonts/inter-latin.woff2 return 200

This will: merge dev into main, tag $tag, push main and the tag.
MSG
read -r -p "Continue? [y/N] " answer
[[ "$answer" == "y" || "$answer" == "Y" ]] || { echo "stopped, nothing pushed"; exit 1; }

git pull --rebase origin dev
git push origin dev
git checkout main
git pull --ff-only origin main
git merge --no-ff dev -m "Release $tag"
git tag -a "$tag" -m "Release $tag"
git push origin main
git push origin "$tag"
git checkout dev

echo "== pushed $tag; GitHub Actions is building the release"
if command -v gh >/dev/null; then
  sleep 5
  gh run watch --exit-status "$(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')" || true
  gh release view "$tag"
fi
