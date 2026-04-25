#!/usr/bin/env bash
# Release helper: bumps the pinned image tag in the Helm chart,
# commits the bump on main, creates an annotated git tag, and prints
# the single push command that fires CI.
#
# Usage:
#   scripts/release.sh 0.1.2
#   scripts/release.sh 0.1.2 "one-line release summary for the tag message"
#
# What it touches:
#   - helm/mdns-controller/values.yaml   (tag: "VERSION")
#   - helm/mdns-controller/Chart.yaml    (version: VERSION, appVersion: "VERSION")
#
# Does NOT push for you. After the script returns clean, run:
#   git push --follow-tags origin main
set -euo pipefail

NEW="${1:-}"
TAG_MSG="${2:-Release v${NEW}}"

if ! [[ "${NEW}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 <X.Y.Z> [\"tag message\"]" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

branch="$(git rev-parse --abbrev-ref HEAD)"
if [ "${branch}" != "main" ]; then
  echo "error: must be on main to cut a release (currently on '${branch}')" >&2
  exit 1
fi

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "error: working tree has uncommitted changes; commit or stash first" >&2
  git status --short >&2
  exit 1
fi

if git rev-parse "v${NEW}" >/dev/null 2>&1; then
  echo "error: tag v${NEW} already exists locally" >&2
  exit 1
fi
if git ls-remote --exit-code --tags origin "v${NEW}" >/dev/null 2>&1; then
  echo "error: tag v${NEW} already exists on origin" >&2
  exit 1
fi

echo "[release] bumping pinned image tag to ${NEW}"

sed -i -E "s|^(  tag: \")[0-9]+\.[0-9]+\.[0-9]+(\")|\1${NEW}\2|" helm/mdns-controller/values.yaml
sed -i -E "s|^(version: )[0-9]+\.[0-9]+\.[0-9]+|\1${NEW}|" helm/mdns-controller/Chart.yaml
sed -i -E "s|^(appVersion: \")[0-9]+\.[0-9]+\.[0-9]+(\")|\1${NEW}\2|" helm/mdns-controller/Chart.yaml

echo "[release] new pins:"
grep -nE "^  tag:" helm/mdns-controller/values.yaml
grep -nE "^(version|appVersion):" helm/mdns-controller/Chart.yaml

echo "[release] validating controller + helm"
(cd controller && go test ./...)
helm lint ./helm/mdns-controller >/dev/null
helm template mdns-controller ./helm/mdns-controller >/dev/null

git add helm/mdns-controller/values.yaml helm/mdns-controller/Chart.yaml
git commit -m "release: pin to v${NEW}"
git tag -a "v${NEW}" -m "${TAG_MSG}"

echo
echo "[release] ready to ship v${NEW}. To publish:"
echo "  git push --follow-tags origin main"
echo
echo "  -> publish-images.yml will tag ghcr.io/shipstuff/mdns-controller:${NEW}"
echo "  -> publish-chart.yml will publish oci://ghcr.io/shipstuff/charts/mdns-controller:${NEW}"
echo
echo "  Reverting before push:"
echo "    git tag -d v${NEW} && git reset --hard HEAD~1"

