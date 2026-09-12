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

Use a Kubernetes context permitted to create the chart's ServiceAccount,
DaemonSet, ClusterRole, and ClusterRoleBinding, and manage Helm release secrets
in the target namespace. Nodes need a multicast-capable LAN interface and an
ingress endpoint reachable at the advertised node address. The default chart
uses host networking, host D-Bus access, and a root container with capabilities;
namespace admission policies must allow these settings.

From this repo:

```bash
helm upgrade --install mdns-controller ./helm/mdns-controller \
  --namespace kube-system
```

From the published OCI chart after a `v*` tag has been pushed and CI has published it:

```bash
helm upgrade --install mdns-controller oci://ghcr.io/shipstuff/charts/mdns-controller \
  --version 0.1.1 \
  --namespace kube-system
```

For the servertimeai/home-lab defaults:

```bash
helm upgrade --install mdns-controller oci://ghcr.io/shipstuff/charts/mdns-controller \
  --version 0.1.1 \
  --namespace kube-system \
  -f examples/values-servertimeai.yaml
```

The chart and image are public. If your Helm client reports a GHCR authentication
error, check for stale registry credentials; authenticated access can use
`helm registry login ghcr.io` with a token allowed to read the package.

### CI Upgrades

An operator can bootstrap the release and grant a separate deployment identity
permission to upgrade it. The chart's read-only Ingress RBAC is for the running
controller, not for Helm or the CI runner. Apply deployment permissions outside
CI; the runner should not be able to modify its own grants.

Upgrading this chart requires access to Helm release secrets and the existing
DaemonSet, ServiceAccount, ClusterRole, and ClusterRoleBinding. Named-resource
permissions can restrict updates, but cannot authorize creation of missing
resources; rerun operator bootstrap if those resources are deleted. Helm's
revisioned secret names also mean standard RBAC cannot restrict secret access
by release label. Choose the release namespace and CI trust boundary accordingly.

### Migrating An Existing Publisher

Use the existing Helm release name and namespace when replacing an in-tree copy
of this chart. Check `helm list -A` and `kubectl get daemonsets -A` first. Remove
the old deployment workflow and any separate legacy publisher after confirming
the new controller is publishing, so both do not advertise the same aliases.
Do not remove host Avahi: the controller intentionally reuses it. Kubernetes
NodeLocal DNSCache (`nodelocaldns`) is unrelated and should also remain.

Check rollout with `kubectl -n kube-system rollout status daemonset/mdns-controller`.
A NotReady node can block full rollout even when the reachable nodes are healthy;
inspect node readiness and controller logs before diagnosing a chart failure.

## Configuration

Common chart values:

| Value | Default | Purpose |
|---|---|---|
| `image.repository` | `ghcr.io/shipstuff/mdns-controller` | Controller image repository. |
| `image.tag` | `0.1.1` | Controller image tag. |
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
| `tolerations` | `[{operator: Exists}]` | Default permits all taints; override to respect custom maintenance taints. |

Cordoning a node does not exclude a DaemonSet. To use a custom `NoSchedule`
maintenance taint, replace the default blanket toleration with only the taints
you intend to tolerate (for example, the control-plane `NoSchedule` taint).
Kubernetes adds standard DaemonSet node-health tolerations automatically. An
offline node cannot confirm termination of an old publisher even after its pod
is removed from desired placement.

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
scripts/release.sh 0.1.2
git push --follow-tags origin main
```

The release script updates the chart version, chart appVersion, and default image tag, commits the bump, and creates an annotated `vX.Y.Z` tag. The tag push publishes:

- `ghcr.io/shipstuff/mdns-controller:X.Y.Z`
- `oci://ghcr.io/shipstuff/charts/mdns-controller:X.Y.Z`
