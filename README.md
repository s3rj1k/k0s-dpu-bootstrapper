# k0s DPU bootstrapper (aka D0kaTr0n)

**What.** A controller that makes DPF-provisioned NVIDIA BlueField DPUs join a **k0s**
cluster.

**Why.** DPF only knows how to emit a `kubeadm join` command. A k0s cluster cannot use one,
so without this a DPU provisions and then never becomes a node.

**How.** DPF's DPU-side agent reads a Secret and runs its `join` key with `bash -c`, without
inspecting it. This controller fills that Secret with a k0s join script instead. No patch or
fork of DPF.

> **Version pinned.** Built against **DPF v26.4.0** (DOCA 26.4). This depends on DPF
> internals, not a public API. Assume a major DPF upgrade breaks it until you have watched a
> DPU join.

## How it works

1. A `DPU` object appears.
2. The controller finds a join script template for that DPU's `DPUCluster`.
3. It mints a k0s worker bootstrap token **in that cluster**.
4. It renders the script and writes it to `<dpu-name>-kubeadm-join` in the DPU's namespace.
5. DPF's agent runs it, and the DPU joins as a node named after the `DPU` object.

DPUs whose cluster has no template are ignored. Many `DPUCluster`s work side by side, each
with its own template and control plane.

## Before you start

### You need

- A **k0s control plane** installed with `--enable-dynamic-config`, or `spec.workerProfiles`
  cannot be applied at runtime.
- A **`DPUCluster`** whose `spec.kubeconfig` Secret holds an admin kubeconfig under
  `super-admin.conf`, with **embedded** `certificate-authority-data`. A file reference fails
  at mint time.
- That kubeconfig's server address **reachable from the DPUs and from inside a Pod**. Worker
  tokens point at the kube-apiserver, not the k0s join API, so `k0sApiPort` need not be
  exposed.
- **One** primary CNI. DPF installs multus, flannel, ovs-cni and nvipam as DPUServices.
  Either k0s runs `network.provider: custom` and flannel is primary, or k0s provides the
  primary and those four are disabled in `DPFOperatorConfig`, which is what the examples do.
- **Working DNS on the DPU.** Without it image pulls fail, and `ntpsec` cannot resolve its
  pool, so the clock stays wrong and every TLS handshake fails as not yet valid.

### Charts to install

cert-manager, because the operator's provisioning manifests create `cert-manager.io`
Certificates and the ApplySet fails without the CRDs. DPF v26.4 pins `v1.19.3`.

```sh
helm repo add jetstack https://charts.jetstack.io --force-update
helm upgrade --install cert-manager jetstack/cert-manager \
  -n cert-manager --create-namespace --version v1.19.3 \
  --set crds.enabled=true --set startupapicheck.enabled=false
```

node-feature-discovery, which publishes the `dpu-enabled` label DPF uses to find hardware.
DPF pins `0.18.3`. Run exactly one NFD. If another chart already ships one, add the affinity
there instead of installing this.

```sh
helm repo add nfd https://kubernetes-sigs.github.io/node-feature-discovery/charts --force-update
helm upgrade --install nfd nfd/node-feature-discovery \
  -n node-feature-discovery --create-namespace --version 0.18.3 \
  --set-json 'worker.affinity={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"node-role.kubernetes.io/dpu","operator":"DoesNotExist"}]}]}}}'
```

The affinity is required. A DPU's own ConnectX reports the same PCI ID as a BlueField.
Without it, NFD running on a DPU node labels that node `dpu-enabled`, DPF treats the DPU as
a DPU host, and schedules a `<node>-dms` Pod on it that crash loops.

The DPF operator.

```sh
helm repo add --force-update dpf-repository https://helm.ngc.nvidia.com/nvidia/doca
helm upgrade --install -n dpf-operator-system dpf-operator dpf-repository/dpf-operator \
  --version v26.4.0 --set kamajiEtcdDefrag.enabled=false
```

`kamajiEtcdDefrag.enabled=false` is required. That Helm value is the only gate on the
CronJob, no `DPFOperatorConfig` field disables it, and without kamaji-etcd its Pod cannot
mount its certificates and stays in `ContainerCreating`.

Not needed: argo-cd, kamaji, local-path-provisioner, maintenance-operator.

## Setup

In order. Every file is in [`examples/`](examples/) and is commented.

**1. Host networking.** [`netplan-br-dpu.yaml`](examples/netplan-br-dpu.yaml) to
`/etc/netplan/` on every DPU host, mode 0600. Without `br-dpu` DPF stops at
`Initialize Interface` with `DPUOOBBridgeNotConfigured`.

**2. k0s worker profile.** The `workerProfiles` block of
[`k0s-cluster-config.yaml`](examples/k0s-cluster-config.yaml). Its profile name must equal
`k0sProfile` in the join script, or the kubelet never starts.

**3. GPU hosts only.** [`containerd-nvidia.toml`](examples/containerd-nvidia.toml) to
`/etc/k0s/containerd.d/`.

**4. Deploy this controller.**

```sh
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deployment.yaml     # set a real image tag first
```

`deploy/deployment.yaml` ships an `image` tag of `dev`, which is **not published**. CI publishes
`latest` and a version tag to `ghcr.io/s3rj1k/k0s-dpu-bootstrapper`.

**5. DPUCluster and its kubeconfig Secret.**
[`dpucluster.yaml`](examples/dpucluster.yaml). Build the Secret with
`kubectl config view --raw --minify --flatten`, where `--flatten` is what embeds the CA.

**6. DPFOperatorConfig.** [`dpfoperatorconfig.yaml`](examples/dpfoperatorconfig.yaml).
Turns the static cluster manager on, kamaji off, and the DPUService backed components off.

**7. Join script ConfigMap.** [`join-script.yaml`](examples/join-script.yaml). Its
annotations must name the DPUCluster from step 5.

**8. Hardware objects.** [`bfb.yaml`](examples/bfb.yaml),
[`dpuflavor.yaml`](examples/dpuflavor.yaml), [`dpuset.yaml`](examples/dpuset.yaml). Set
`dpuNodeSelector` to your own hosts. The DPUSet uses `nodeEffect: hold`, which parks the DPU
after the join Secret is rendered and before anything is flashed.

**9. Verify, then release the hold.**

## Verify

Read the script DPF will run, before it runs.

```sh
kubectl get secret <dpu>-kubeadm-join -n dpf-operator-system \
  -o jsonpath='{.data.join}' | base64 -d
```

Confirm it is this controller's, not DPF's.

```sh
kubectl get secret <dpu>-kubeadm-join -n dpf-operator-system \
  -o jsonpath='{.metadata.annotations}'
# k0s.mirantis.com/managed-by, /join-script-template, /token-expires-at, /token-id
```

Failures are Warning Events on the `DPU`, not on the Secret.

```sh
kubectl describe dpu <dpu> -n dpf-operator-system
```

Release the hold, then watch it through to `Ready`.

```sh
kubectl annotate dpunodemaintenance <node>-hold -n dpf-operator-system \
  provisioning.dpu.nvidia.com/wait-for-external-nodeeffect=false --overwrite
kubectl get dpu -A -w
kubectl get nodes
```

Expect the host to reboot during provisioning, `nodeRebootMethod` defaults to `hostAgent`.

## Join script template

A ConfigMap in `--template-namespace`. The controller finds it by label and matches it to a
DPU by annotation. No match and the DPU is skipped. Two matches of equal specificity and the
controller emits a Warning Event and writes nothing.

| | |
|---|---|
| label | `k0s.mirantis.com/dpu-join-script: "true"` |
| annotations | `k0s.mirantis.com/dpu-cluster-name`, `k0s.mirantis.com/dpu-cluster-namespace` |
| | `k0s.mirantis.com/dpu-flavor`, optional, wins over a cluster-wide template |
| `join.sh` | the script template |
| any other key | a value, exposed as `.Values.<key>` |

Also available: `.JoinToken`, `.TokenExpiresAt`, `.APIServerURL`, `.NodeName`, `.DPUName`,
`.DPUNamespace`, `.ClusterName`, `.ClusterNamespace`.

Three rules:

- Referencing an unset key **fails the render**. There is no empty substitution, because the
  output runs as root on a DPU.
- Values are always **strings**. Compare them, `{{ if eq .Values.foo "true" }}`.
- Values are substituted **verbatim, not rendered**. A `{{ }}` inside a value reaches the
  script as literal text, and only the first line of a multi-line value is indented.

Branch on `.DPUName` for a one DPU exception. Use a flavor scoped template for a group.

The DPU agent behaves in four ways a script must accommodate. Ignoring any one stalls
provisioning.

- **The whole script re-runs on failure**, every 30 seconds. Every step must be idempotent.
- **Neutralise the systemd kubelet, do not mask it.** A later agent step starts it on every
  boot, and `start` on a masked unit fails forever.
- **Leave `/var/lib/kubelet/config.yaml` parseable.** The agent reads it after the join, and
  k0s writes no such file.
- **Leave a `kubelet` on `PATH`** that prints `Kubernetes vX.Y.Z`. The agent runs
  `kubelet --version` and requires that prefix.

## Flags

| flag | default | |
|---|---|---|
| `--template-namespace` | `dpf-operator-system` | where join script ConfigMaps are read from |
| `--token-ttl` | `4h` | validity of a minted bootstrap token |
| `--token-refresh-before` | `1h` | mint again once a token has this much left. Must be shorter than `--token-ttl`, or the process exits at startup |
| `--leader-elect` | `true` | only one replica mints tokens |
| `--kubeconfig` | unset | unset uses the in-cluster config, otherwise this flag then `$KUBECONFIG` |
| `--health-probe-bind-address` | `:8081` | |
| `--version` | | print the version and exit |

The controller-runtime `--zap-*` logging flags are also registered.
`deploy/deployment.yaml` already sets the first four.

## Troubleshooting

| Symptom | Cause |
|---|---|
| DPU stuck at `Initialize Interface`, `DPUOOBBridgeNotConfigured` | no `br-dpu` on the host, step 1 |
| DPU stuck at `DPU Cluster Config`, `NodeNotFound` | the join script ran but no node registered. Read the rendered Secret |
| kubelet never starts on the DPU | `k0sProfile` names a worker profile that does not exist |
| TLS `certificate is not yet valid` | the DPU clock is behind, usually no DNS so `ntpsec` cannot reach its pool |
| `<node>-dms` Pod crash loops on a DPU node | NFD labelled the DPU `dpu-enabled`. Apply the worker affinity |
| CronJob stuck in `ContainerCreating` | `kamajiEtcdDefrag.enabled=false` was not set |
| `DPUDevice.status.pciAddress` holds the DPU's own numbering | `dpu-detector` ran on a DPU node. Delete the DPUDevice, restart the `<node>-dms` Pod, keep the detector off DPU nodes |
| Nothing happens and there are no Events | the DPUSet selector matched no DPUNode, so no `DPU` object exists |

## Development

```sh
go install github.com/go-task/task/v3/cmd/task@latest
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

```sh
task              # list tasks
task verify       # lint and tests, what CI runs
task build        # ./bin/bootstrapper
task image TAG=v0.1.0
task deploy       # kubectl apply of deploy/
```

The image is a static binary on `gcr.io/distroless/static-debian12:nonroot`, uid 65532.

## Design notes

**No CRDs.** Configuration is a labelled ConfigMap, so installing this adds nothing to the
cluster's API surface. There is no status subresource, so failures are Warning Events on the
DPU and the annotations on the Secret record the last **successful** render.

**Overwriting DPF's Secret is safe.** DPF writes it once and never updates it, in zero-trust
mode the agent may read only a Secret of that name, and its owner reference deletes it with
the DPU.

**Token lifetime.** The token must be valid from minting until the agent runs the script, a
window that includes BFB flashing. Nothing reads it afterwards, because k0s auto-approves
kubelet certificate rotation.

**No DPF import.** Its objects are read unstructured and projected in `internal/dpf`, making
DPF a runtime rather than a compile-time dependency.

**No k0s import.** `internal/k0stoken` is derived from k0s `pkg/token` (Apache-2.0, headers
retained), because importing it pulls in `k8s.io/kubernetes` and ~31 replace directives.
Diff it against upstream when upgrading k0s.

**Known risk.** This controller owns the contents of a Secret DPF writes for its own use, a
real but undeclared contract. Pin the DPF version in CI with a test that asserts a DPU still
joins.
