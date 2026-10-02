---
layout: default
nav_title: Experimental DRA
nav_order: 8
---

# Experimental whole-GPU DRA prototype

This is an opt-in provider-side prediction for explicit `ResourceClaim` workloads.
It is not a validated LKE/NVIDIA integration. Verification is limited to local
unit tests with mocked APIs and Helm template fixtures; no cluster, provisioning,
or end-to-end tests are included in this change or required by its unit-test target.

## What is modeled

For an explicitly allowlisted Linode instance type with a positive API `GPUs`
count, the provider predicts that many independent devices for `gpu.nvidia.com`.
Each device has only the string attribute `type: gpu`. Pool and device names are
simulation-local identifiers, not predictions of runtime node names or GPU minors.
CPU, host memory, and pod capacity remain unchanged.

The administrator's allowlist is an assertion that the selected type is available
to the account/region, eligible for the chosen LKE tier, and configured to publish
**every GPU as an exclusive whole GPU** under this driver. The Linode API's GPU
count alone proves none of those compatibility prerequisites. Unknown/unlisted
catalog types and types with no GPUs produce no DRA inventory; accelerated-device
counts are not treated as GPU counts. There is no built-in list of certified SKUs.

Karpenter v1.14.1 supplies the scheduling simulation and narrows the instance types
passed to the existing launch path. Kubernetes and the installed NVIDIA driver
perform real claim allocation and device preparation. This prototype creates no
DRA driver, allocator, CRD, ResourceSlice, or claim allocation.

## Opt in

The default empty allowlist leaves provider DRA predictions disabled. After
verifying the prerequisites below, configure exact type IDs in Helm values:

```yaml
settings:
  mode: lke
  dra:
    nvidiaGPUInstanceTypes:
      - "REPLACE-WITH-A-VERIFIED-LKE-GPU-TYPE-ID"
```

A nonempty list renders both `NVIDIA_DRA_INSTANCE_TYPES` and the core setting
`IGNORE_DRA_REQUESTS="false"`, plus only `get`, `list`, and `watch` permissions on
`resource.k8s.io` `resourceclaims`, `resourceslices`, and `deviceclasses`.
The chart rejects instance mode and duplicate DRA overrides in `controller.env`.
The chart does not install NVIDIA software or change cluster feature gates.

For a controller deployed without this chart, configure both
`--nvidia-dra-instance-types=<comma-separated-exact-IDs>` (or
`NVIDIA_DRA_INSTANCE_TYPES`) and `--ignore-dra-requests=false` (or
`IGNORE_DRA_REQUESTS=false`), and supply the same read permissions. A nonempty
allowlist with core DRA disabled is rejected before contacting Linode. Instance
mode is rejected because it does not yet bootstrap/join Kubernetes nodes.

This configuration is controller-wide and fixed until restart. All NodeClasses
using an allowlisted type must have the same whole-GPU DRA bootstrap profile;
do not use the same selected SKU for a separate legacy GPU installation. Use
NodePool instance-type requirements to constrain workloads to the verified plans.
Changing the allowlist and restarting discards cached predictions, but does not
reconfigure existing nodes or claims.

## Prerequisites and unverified runtime behavior

The researched reference is
[NVIDIA DRA v0.5.0](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/v0.5.0),
not an installed dependency or a certified compatibility matrix. Its
[GPU prerequisites](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.0/site/content/docs/prerequisites.md)
include Kubernetes **1.34.2 or newer**, a compatible NVIDIA driver (direct driver
minimum 565; GPU Operator workflow minimum 580), container toolkit 1.18 or newer,
a CDI-enabled runtime, and the driver's discovery labels. Particular GPUs may
require newer versions. LKE support for a GPU does not itself establish NVIDIA
DRA support for that GPU; verify hardware eligibility as well as software.

Verify the actual API server and worker patch versions and the availability of
`resource.k8s.io/v1`. LKE Enterprise's
[1.34 release](https://techdocs.akamai.com/cloud-computing/changelog/jul-7-2026-lke-enterprise-kubernetes-v134-support)
enables base DRA, but a displayed minor version alone does not establish the
NVIDIA patch minimum. GPU software also differs by LKE tier; consult the
[GPU-on-LKE guide](https://techdocs.akamai.com/cloud-computing/docs/gpus-on-lke).
Missing DRA APIs or read permissions can disrupt ordinary scheduling batches once
core DRA is enabled.

Use an exclusive, non-MIG, non-sharing NVIDIA profile. Disable ComputeDomains
(enabled by default in the reference standalone chart), and disable the legacy
NVIDIA device plugin for the same devices to avoid independent allocators owning
one GPU. Use a DeviceClass that selects `gpu.nvidia.com` and `type == "gpu"`.
Verify that complete node-local ResourceSlice pools include correct Node owner
references and contain the API-reported number of usable whole GPUs.

Initialization for a DRA-requesting NodeClaim waits for the requested driver to
publish a complete pool. That check does not compare actual device counts or
attributes with the prediction. A GPU node launched for a CPU-only workload has
no requested-driver annotation and can initialize before DRA publication; later
GPU workloads may cause unnecessary additional provisioning. A separately
implemented, driver-controlled bootstrap/readiness mechanism is needed to close
that window. Merely adding a NodePool `startupTaint` is insufficient on LKE:
the provider does not inject arbitrary startup-only taints into LKE pools.
This prototype does not add such a bootstrap mechanism. Incorrect predictions
can also affect consolidation/replacement simulation.

## Boundaries

- Explicit whole-GPU claims only; no legacy `nvidia.com/gpu` capacity or implicit
  extended-resource-to-DRA bridge support
- No product, GPU memory, architecture, UUID, PCI/NUMA/fabric topology, or bindings
  are predicted. Host RAM and SKU label strings are not GPU metadata. Selectors
  requiring absent attributes/capacity cannot be satisfied from these templates
- No MIG, sharing, VFIO, ComputeDomains, admin access, or device-taint guarantees
- Pinned core rejects `FirstAvailable` and `DistinctAttribute`; a newer Kubernetes
  API schema does not add these allocator features
- Real allocation, kubelet preparation, scale-up, bootstrap, and disruption
  behavior on actual LKE hardware remain unverified

## Mock-only unit checks

With the repository's pinned Go and Helm tools available, run:

```sh
just test-dra-unit
# Equivalent command; the anchored name filter excludes existing envtest suites:
go test ./pkg/operator/options ./pkg/operator ./pkg/providers/instancetype -run '^TestDRA' -count=1
```

The tests cover explicit opt-in, exact whole-GPU inventory, API-count refresh,
List/Get/cache/offering propagation, core CEL matching and exclusive allocation
with a fake DeviceClass client, option validation, and rendered environment
variables and least-privilege RBAC. The catalog and operator calls are mocked;
Helm only renders local files with fixture values. Do not substitute the general
`just test` recipe: it starts envtest and is outside this prototype's test scope.

The checks live beside the code they exercise: `pkg/operator/options` covers
flags and Helm rendering, `pkg/operator` covers startup validation, and
`pkg/providers/instancetype/dynamicresources_test.go` covers inventory, caching,
and allocation together. They use the same external test packages and Linode
fake as the existing suites. Standalone Go unit tests follow the repository's
existing unit-test pattern and keep these checks outside the Ginkgo suites'
envtest lifecycle. Helm renders fixture values into a temporary directory; it
never installs the chart.

Linode tests create a fresh `fake.NewLinodeClient` per case, configure shared
`AtomicPtr` fixtures and `MockedFunction.Error`, assert `Calls`/`CalledWithInput`,
and register `Reset`/cache `Flush` with `t.Cleanup`. Thin local adapters add
behavior hooks to catalog methods whose shared fake implementation has none;
they do not implement a second Linode fake. The allocator's NodeClaim adapter
remains local because pinned core's equivalent test fixture is unexported.
Its DeviceClass client is an isolated controller-runtime in-memory fake; no
API-server environment is created.
