# mDNS Controller Helm Chart

Installs the node-local mDNS controller as a DaemonSet. The chart defaults to `kube-system`, host networking, host Avahi reuse, and bundled Avahi fallback.

## Install

```bash
helm upgrade --install mdns-controller ./helm/mdns-controller \
  --namespace kube-system
```

From the published OCI chart:

```bash
helm upgrade --install mdns-controller oci://ghcr.io/shipstuff/charts/mdns-controller \
  --version 0.1.0 \
  --namespace kube-system
```

## Common Overrides

Opt-in only publication:

```bash
helm upgrade --install mdns-controller ./helm/mdns-controller \
  --namespace kube-system \
  --set ingress.defaultEnabled=false
```

Force a LAN interface:

```bash
helm upgrade mdns-controller ./helm/mdns-controller \
  --namespace kube-system \
  --reuse-values \
  --set network.interface=eth0
```

Use a private registry image:

```yaml
image:
  repository: registry.local:30500/networking/mdns-controller
  tag: "0.1.0"
```

## Values

See [`values.yaml`](values.yaml) for the full set of supported values.

