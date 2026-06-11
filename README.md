# vault-operator-helper

A small sidecar that keeps a ConfigMap in sync with the set of namespaces
matching a label selector, and restarts a co-located "main" container whenever
that set changes.

It is intended to run alongside a container (such as Vault) that reads a
comma-separated list of namespaces from a ConfigMap (for example via a
`WATCH_NAMESPACE` key) and only picks up changes on restart.

## How it works

1. On startup it builds a Kubernetes client (in-cluster config, falling back to
   a local kubeconfig).
2. It starts a shared informer that watches **namespaces** matching
   `--label-selector`.
3. Whenever a matching namespace is added, updated, or deleted, it recomputes
   the sorted namespace list and writes it to the ConfigMap under
   `--watch-key`.
4. If (and only if) the value actually changed, it sends `SIGTERM` to the main
   container's process so it reloads the new configuration.

The namespace list is read from the informer's in-memory cache (no extra API
calls per event) and sorted, so the ConfigMap value is stable and does not
flip — and trigger needless restarts — due to cache iteration order.

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-configmap-name` | `watch-namespace-config` | Name of the ConfigMap to update |
| `-configmap-namespace` | `vault` | Namespace of the ConfigMap |
| `-label-selector` | `foo=bar` | Label selector for namespaces to watch |
| `-watch-key` | `WATCH_NAMESPACE` | Key in the ConfigMap holding the namespace list |
| `-main-container` | `main-container` | Process name of the main container to restart |
| `-health-addr` | `:8081` | Address for the liveness probe server |

## Health probe

The process serves a liveness endpoint on `--health-addr`:

- `GET /healthz` — returns `200` once the process is running.

Example probe configuration:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8081
```

## Deployment notes

### Shared process namespace

The restart works by signalling the main container's process directly, so the
two containers must share a process namespace. Set this on the pod spec:

```yaml
spec:
  shareProcessNamespace: true
```

`-main-container` must match a string in the target process's command line.

### Running as root

The helper signals a process that belongs to another container. Across a shared
process namespace this generally requires matching privileges, so the image
runs as root by design. Switching to an unprivileged user will break the
restart on most setups.

### RBAC

The helper needs to list namespaces and to get/create/update the ConfigMap.
A minimal example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: vault-operator-helper
rules:
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: vault-operator-helper
  namespace: vault
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "create", "update"]
```

Bind both to the ServiceAccount used by the pod.

## Image

Images are published to `ghcr.io/hsiaoairplane/vault-operator-helper`.

## Development

```sh
make fmt    # gofmt
make vet    # go vet
make test   # go test
make build  # build the binary (-race)
```
