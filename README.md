# mDNS Controller

Kubernetes controller that publishes ingress-backed `.local` hostnames through Avahi on every node. It is intended for home and lab networks where ingress hostnames like `llm.local` or `vault.local` should resolve to a node-local LAN address via mDNS.

## What It Does

- runs as a DaemonSet so every node advertises the same ingress-backed `.local` names
- watches `networking.k8s.io/v1` Ingress objects with read-only RBAC
- filters `spec.rules[].host` values to names ending in `.local` by default
- supports opt-out or opt-in annotation policy through `mdns.shipstuff.io/enabled`
- reuses host Avahi over host D-Bus when available
- falls back to a bundled `dbus-daemon` plus `avahi-daemon` when host Avahi is unavailable
- publishes each alias independently with `avahi-publish-address -R`, so one collision does not block the full set
- exposes `/healthz`, `/readyz`, `/metrics`, and `/state` on port `8080`

## Install

From this repo:

```bash
helm upgrade --install mdns-controller ./helm/mdns-controller \
  --namespace kube-system
```

From the published OCI chart after a `v*` tag has been pushed and CI has published it:

```bash
helm upgrade --install mdns-controller oci://ghcr.io/shipstuff/charts/mdns-controller \
  --version 0.1.0 \
  --namespace kube-system
```

For the servertimeai/home-lab defaults:

```bash
helm upgrade --install mdns-controller ./helm/mdns-controller \
  --namespace kube-system \
  -f examples/values-servertimeai.yaml
```

## Configuration

Common chart values:

| Value | Default | Purpose |
|---|---|---|
| `image.repository` | `ghcr.io/shipstuff/mdns-controller` | Controller image repository. |
| `image.tag` | `0.1.0` | Controller image tag. |
| `namespaceOverride` | `kube-system` | Namespace used by rendered resources. |
| `hostAvahi.enabled` | `true` | Reuse host Avahi through `/run/dbus` when available. |
| `bundledAvahi.enabled` | `true` | Start bundled D-Bus and Avahi fallback when needed. |
| `ingress.hostSuffix` | `.local` | Host suffix eligible for mDNS publication. |
| `ingress.annotationKey` | `mdns.shipstuff.io/enabled` | Annotation key controlling inclusion. |
| `ingress.defaultEnabled` | `true` | Include matching hosts unless annotated `false`; set `false` for opt-in. |
| `network.interface` | empty | Force the LAN interface used for advertisement. |
| `network.address` | empty | Force the IPv4 address registered for aliases. |
| `network.extraExcludeInterfaces` | empty | Extra comma-separated Avahi deny-interface patterns. |
| `health.reconcileInterval` | `15s` | Kubernetes-to-Avahi reconcile interval. |
| `health.staleAfter` | `60s` | Readiness staleness threshold after last successful reconcile. |

When `ingress.defaultEnabled=true`, an ingress is included unless it has:

```yaml
metadata:
  annotations:
    mdns.shipstuff.io/enabled: "false"
```

When `ingress.defaultEnabled=false`, an ingress is excluded unless it has:

```yaml
metadata:
  annotations:
    mdns.shipstuff.io/enabled: "true"
```

## Runtime Notes

mDNS is multicast, not authoritative DNS. It is limited to the local broadcast domain unless your network explicitly reflects mDNS between segments. `.local` names can also collide with other Bonjour/Avahi responders. The controller treats collisions as per-host publication failures and continues publishing the rest of the desired aliases.

Each node publishes aliases to its own LAN-facing IPv4 address. This is useful for home-network ingress, but it is not deterministic load balancing.

## Build

```bash
docker build -t ghcr.io/shipstuff/mdns-controller:dev .
```

The Dockerfile builds the Go controller statically, then packages it with Avahi, D-Bus, `iproute2`, and the entrypoint script.

## Release

Release flow matches the `windrose-self-hosted` pattern:

```bash
scripts/release.sh 0.1.1
git push --follow-tags origin main
```

The release script updates the chart version, chart appVersion, and default image tag, commits the bump, and creates an annotated `vX.Y.Z` tag. The tag push publishes:

- `ghcr.io/shipstuff/mdns-controller:X.Y.Z`
- `oci://ghcr.io/shipstuff/charts/mdns-controller:X.Y.Z`

