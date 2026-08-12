# k0s DPU bootstrapper

Makes DPF-provisioned NVIDIA BlueField DPUs join a **k0s** cluster instead of a kubeadm one,
without patching or forking DPF.

DPF's DPU-side agent reads a Secret and runs the value of its `join` key with `bash -c`,
without inspecting it. DPF fills that Secret with a `kubeadm join` command; this controller
fills it with a rendered k0s join script instead.

## How it works

For every `DPU` whose `DPUCluster` has a join script template, the controller mints a k0s
worker bootstrap token in that cluster, renders the script around it, and writes the result
to `<dpu-name>-kubeadm-join` in the DPU's namespace.

That is DPF's own join Secret, not a second one. DPF creates it once and never updates it,
in zero-trust mode the agent may read only a Secret of that name, and its owner reference
garbage collects it with the DPU. A DPU whose cluster has no template is left alone, and
many `DPUCluster`s work side by side, each with its own template and control plane.

The token only has to be valid between minting and the agent running the script, a window
that includes BFB flashing. It is never read again afterwards, because k0s auto-approves the
kubelet's own certificate rotation.

## The join script template

A ConfigMap in `--template-namespace`, discovered by label and scoped by annotation. Zero
matches means "not ours", two equally specific matches is an error raised as an Event.

| | |
|---|---|
| label | `k0s.mirantis.com/dpu-join-script: "true"` |
| annotations | `k0s.mirantis.com/dpu-cluster-name`, `k0s.mirantis.com/dpu-cluster-namespace` |
| | `k0s.mirantis.com/dpu-flavor`, optional, wins over a cluster-wide template |
| `join.sh` | the script template |
| any other key | a value, exposed as `.Values.<key>` |

Also available: `.JoinToken`, `.TokenExpiresAt`, `.APIServerURL`, `.NodeName`, `.DPUName`,
`.DPUNamespace`, `.ClusterName`, `.ClusterNamespace`.

Three rules, all of which bite:

- Referencing something unset **fails the render**, since the result runs as root on a DPU.
- Values are always **strings** — compare them, `{{ if eq .Values.foo "true" }}`.
- Values are substituted **verbatim, not rendered**, so a value cannot contain `{{ … }}`,
  and a multi-line one only has its first line indented.

`.DPUName` lets one template carry a per-DPU exception; a flavor-scoped template is the
answer for a whole group.

## The examples

[`examples/join-script.yaml`](examples/join-script.yaml) runs against stock DPF, so it also
undoes the kubelet work the agent does around it. Every step is a key of its own, and the
skeleton exports the controller's facts as shell variables so each step is plain bash.

Four agent behaviours shape it, and each one wedges provisioning if ignored:

- **The whole script re-runs on failure**, every 30 seconds, install and join included.
- **Neutralise the systemd kubelet, do not mask it.** A later agent step starts it on every
  boot, and `start` on a masked unit fails forever.
- **Leave `/var/lib/kubelet/config.yaml` valid.** The agent parses it afterwards, and k0s
  writes no such file.
- **Leave `/usr/bin/kubelet` in place.** The agent reads its version after the join.

[`examples/k0s-cluster-config.yaml`](examples/k0s-cluster-config.yaml) is the control plane
half: the worker profile the script joins with, and the CNI and NUMA settings.

## Requirements

- A `DPUCluster` whose `spec.kubeconfig` Secret holds an admin kubeconfig under
  `super-admin.conf`, with embedded `certificate-authority-data` and one cluster entry.
- That address reachable from the DPUs. A worker token points at the kube-apiserver, not
  the k0s join API, so no extra endpoint is needed.
- Agreement on the CNI. DPF installs multus, flannel, ovs-cni and nvipam as DPUServices, so
  either k0s runs `network.provider: custom` and flannel is primary, or k0s provides the
  primary and flannel is disabled in `DPFOperatorConfig`. Two primaries will fight.

## Flags

| flag | default | |
|---|---|---|
| `--template-namespace` | `dpf-operator-system` | where join script ConfigMaps are read from |
| `--token-ttl` | `4h` | validity of a minted bootstrap token |
| `--token-refresh-before` | `1h` | mint again once a token has this much left |
| `--leader-elect` | `true` | only one replica mints tokens |
| `--kubeconfig` | unset | unset means in-cluster; `KUBECONFIG` is honoured in between |
| `--health-probe-bind-address` | `:8081` | |

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

**No CRDs.** Configuration is a labelled ConfigMap, so nothing is installed into a cluster
whose lifecycle we do not own. The cost is no status subresource, so failures surface as
Events and as annotations on the Secret.

**No DPF import.** Its objects are read unstructured and projected in `internal/dpf`, making
DPF a runtime rather than a compile-time dependency.

**No k0s import.** `internal/k0stoken` is derived from k0s `pkg/token` (Apache-2.0, headers
retained), because importing it pulls in `k8s.io/kubernetes` and ~31 replace directives.
Diff it against upstream when upgrading k0s.

**Known risk.** This controller owns the contents of a Secret DPF writes for its own use, a
real but undeclared contract. Pin the DPF version in CI with a test that asserts a DPU still
joins.
