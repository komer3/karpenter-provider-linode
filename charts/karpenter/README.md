# LKE Karpenter provider

A Helm chart for LKE Karpenter provider

## Documentation

For full Karpenter documentation please checkout [https://karpenter.sh](https://karpenter.sh/v0.32.1/).

## Experimental NVIDIA DRA

`settings.dra.nvidiaGPUInstanceTypes` defaults to `[]`. A nonempty list of
operator-verified LKE type IDs opts into whole-GPU predictions, enables core DRA
scheduling, and grants read-only access to claims, slices, and device classes.
It does not install a driver. Read the [scope and prerequisites](../../docs/DRA.md)
before enabling this unverified prototype.
