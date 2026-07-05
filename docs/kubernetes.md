# MedVFS — Kubernetes Deployment Guide

> **Source:** `deployments/helm/medvfs/`

---

## Architecture Overview

MedVFS runs as a **privileged FUSE daemon** in Kubernetes. It exposes remote medical data (DICOM, WSI, Genomics) as local files that ML training containers read transparently.

```
┌─────── Kubernetes Node ────────────────────────┐
│                                                │
│  ┌──── MedVFS Pod ────┐    ┌── ML Pod ────────┐│
│  │ medvfs serve --fuse │    │ python train.py  ││
│  │   ↓                │    │   open("/data/…") ││
│  │ FUSE mount ────────┼────┼──→ /data/medvfs/  ││
│  │ BlockManager       │    │                   ││
│  │ Prefetcher         │    └───────────────────┘│
│  │ HTTP API :8090     │                         │
│  └────────────────────┘                         │
│                                                 │
│  /dev/fuse (host device)                        │
│  /data/medvfs (shared mount, Bidirectional)     │
│  /var/cache/medvfs (block cache)                │
└─────────────────────────────────────────────────┘
```

---

## Deployment Modes

### 1. DaemonSet (multi-pod shared access)

MedVFS runs on every selected node. The FUSE mount is exposed to the host via **Bidirectional mount propagation**, making `/data/medvfs` accessible to all pods on that node.

**Best for:** clusters with multiple ML pods per node that share the same dataset.

```bash
helm install medvfs deployments/helm/medvfs/ \
  --set mode=daemonset \
  --set hostFuse.mountPath=/data/medvfs \
  --set-file configInline=my-vfs.yaml
```

**How it works:**
1. DaemonSet deploys one MedVFS pod per node (filtered by `nodeSelector`)
2. MedVFS mounts FUSE at `/data/vfs` inside the container
3. Bidirectional propagation exposes the mount at `hostFuse.mountPath` on the host
4. Other pods mount the same `hostPath` to access files

**ML Pod manifest:**
```yaml
spec:
  volumes:
    - name: medvfs
      hostPath:
        path: /data/medvfs
        type: Directory
  containers:
    - name: training
      volumeMounts:
        - name: medvfs
          mountPath: /data/vfs
          readOnly: true
```

### 2. Sidecar (per-pipeline isolation)

MedVFS runs alongside the ML container in the same pod, sharing a FUSE mount via `emptyDir`.

**Best for:** isolated pipelines where each job needs its own VFS configuration.

```bash
helm template medvfs deployments/helm/medvfs/ \
  --set mode=sidecar \
  --set-file configInline=my-vfs.yaml \
  > medvfs-sidecar.yaml
```

Then add the MedVFS container spec to your ML pod:

```yaml
spec:
  volumes:
    - name: fuse-mount
      emptyDir: {}
    - name: dev-fuse
      hostPath:
        path: /dev/fuse
    - name: cache
      emptyDir: {}
  containers:
    # ── Your ML container
    - name: training
      image: my-ml-image:latest
      volumeMounts:
        - name: fuse-mount
          mountPath: /data/vfs
          mountPropagation: HostToContainer

    # ── MedVFS sidecar
    - name: medvfs
      image: ghcr.io/colabbio/medvfs:0.1.0
      securityContext:
        privileged: true
      args: ["serve", "--fuse", "--mount-point=/data/vfs", "-c=/etc/medvfs/vfs.yaml"]
      volumeMounts:
        - name: fuse-mount
          mountPath: /data/vfs
          mountPropagation: Bidirectional
        - name: dev-fuse
          mountPath: /dev/fuse
        - name: cache
          mountPath: /var/cache/medvfs
```

### 3. Standalone (dev/testing)

A single Deployment for development and testing. No host mount propagation needed.

```bash
helm install medvfs-dev deployments/helm/medvfs/ \
  --set mode=standalone \
  --set cache.type=emptyDir \
  --set-file configInline=my-vfs.yaml
```

---

## Mode Comparison

| Feature | DaemonSet | Sidecar | Standalone |
|---|---|---|---|
| **Pods per node** | 1 per node | 1 per ML pod | 1 total |
| **Mount sharing** | All pods on node | Same pod only | Pod-internal |
| **Cache** | `hostPath` (persists) | `emptyDir` (ephemeral) | `PVC` or `emptyDir` |
| **Isolation** | Shared config | Per-pipeline | Single config |
| **Security** | `privileged: true` | `privileged: true` | `privileged: true` |
| **Use case** | Multi-pipeline nodes | CI/CD, Nextflow | Dev, debugging |

---

## Helm Chart Reference

### Installation

```bash
# Add config as inline YAML
helm install medvfs deployments/helm/medvfs/ \
  --set-file configInline=path/to/vfs.yaml

# Or reference an existing ConfigMap
helm install medvfs deployments/helm/medvfs/ \
  --set existingConfigMap=my-vfs-config
```

### Key Values

| Value | Default | Description |
|---|---|---|
| `mode` | `daemonset` | Deployment mode (`daemonset` / `sidecar` / `standalone`) |
| `image.repository` | `ghcr.io/colabbio/medvfs` | Container image |
| `image.tag` | `0.1.0` | Image version |
| `vfs.mountPoint` | `/data/vfs` | FUSE mount path inside container |
| `vfs.cacheDir` | `/var/cache/medvfs` | Block cache directory |
| `vfs.cacheMaxGB` | `50` | Maximum cache size |
| `vfs.blockSizeMB` | `4` | Block size (MB) |
| `vfs.port` | `8090` | HTTP API port |
| `vfs.prefetch.enabled` | `true` | Enable prefetch engine |
| `vfs.prefetch.strategy` | `format_aware` | Prefetch strategy |
| `cache.type` | `hostPath` | Cache volume type (`hostPath`/`pvc`/`emptyDir`) |
| `cache.hostPath` | `/var/cache/medvfs` | Host cache path (DaemonSet) |
| `cache.pvc.size` | `100Gi` | PVC size (standalone/sidecar) |
| `hostFuse.mountPath` | `/data/medvfs` | Host FUSE mount (DaemonSet) |
| `healthCheck.enabled` | `true` | Enable liveness/readiness probes |
| `metrics.enabled` | `false` | Enable Prometheus metrics port |

### Validation

```bash
# Lint the chart
make helm-lint
# or: helm lint deployments/helm/medvfs/

# Render templates (DaemonSet mode)
make helm-template
# or: helm template medvfs deployments/helm/medvfs/

# Render templates (Sidecar mode)
make helm-template-sidecar
# or: helm template medvfs deployments/helm/medvfs/ --set mode=sidecar
```

---

## Security & RBAC

### FUSE Requirements

FUSE filesystems require elevated privileges on Kubernetes:

```yaml
securityContext:
  privileged: true
```

**Or** (more restrictive):

```yaml
securityContext:
  capabilities:
    add: ["SYS_ADMIN"]
  seccompProfile:
    type: Unconfined
```

> **Note:** Most managed K8s platforms (GKE, EKS, AKS) support `privileged: true` for DaemonSets with appropriate PodSecurityPolicies or PodSecurityStandards.

### Device Access

The `/dev/fuse` host device must be mounted into the MedVFS container:

```yaml
volumes:
  - name: dev-fuse
    hostPath:
      path: /dev/fuse
```

### Mount Propagation

Bidirectional mount propagation is required for DaemonSet mode to make the FUSE mount visible to other pods:

```yaml
volumeMounts:
  - name: fuse-mount
    mountPath: /data/vfs
    mountPropagation: Bidirectional  # MedVFS container
```

Consumer pods use `HostToContainer`:

```yaml
volumeMounts:
  - name: medvfs
    mountPath: /data/vfs
    mountPropagation: HostToContainer  # ML container
```

### Network Policies

MedVFS requires egress to data sources:

| Destination | Port | Protocol | Purpose |
|---|---|---|---|
| PACS / Orthanc | 8042, 443 | HTTPS | DICOMweb WADO-RS |
| OMERO.web | 443 | HTTPS | Image retrieval |
| PostgreSQL (OMOP) | 5432 | TCP | SQL queries |
| FHIR server | 443 | HTTPS | FHIR R4 API |
| GCS / S3 | 443 | HTTPS | Genomics file access |

---

## Production Checklist

### Before Deployment

- [ ] Verify `/dev/fuse` is available on target nodes
- [ ] Test mount propagation: `kubectl exec -it <pod> -- mount | grep fuse`
- [ ] Validate `vfs.yaml` connectivity: `medvfs validate -c vfs.yaml`
- [ ] Set appropriate `nodeSelector` for GPU/ML nodes
- [ ] Configure PodDisruptionBudget for DaemonSet rolling updates

### Cache Sizing

| Dataset Type | Recommended `cacheMaxGB` | Block Size |
|---|---|---|
| DICOM (CT/MR) | 50–100 GB | 4 MB |
| WSI (SVS/NDPI) | 100–500 GB | 4 MB |
| Genomics (BAM) | 200–1000 GB | 4 MB |
| Mixed multimodal | 200 GB | 4 MB |

**Rule of thumb:** cache should be 20-30% of total dataset size for good hit rates.

### Monitoring

```bash
# Check cache stats
curl http://<service>:8090/stats

# Watch live events
curl -N http://<service>:8090/events

# View manifest
curl http://<service>:8090/manifest | jq '.count'

# Kubernetes health
kubectl get pods -l app.kubernetes.io/name=medvfs
kubectl logs -l app.kubernetes.io/name=medvfs --tail=50
```

---

## Nextflow Integration

For Nextflow-orchestrated pipelines, MedVFS runs in a DaemonSet and Nextflow tasks mount the shared FUSE path:

```groovy
// nextflow.config
process {
    pod {
        volumeMount {
            name     = 'medvfs'
            mountPath = '/data/vfs'
        }
        volume {
            name     = 'medvfs'
            hostPath = '/data/medvfs'
        }
    }
}
```

```groovy
// pipeline.nf
process INFERENCE {
    input:
    path slide from channel.fromPath("/data/vfs/pathology/*.svs")

    script:
    """
    python inference.py --input ${slide}
    """
}
```

---

## CSI Driver (Future)

The Helm chart includes a skeleton CSI (Container Storage Interface) driver for future Kubernetes-native volume provisioning. When complete, this will allow mounting MedVFS volumes declaratively:

```yaml
volumes:
  - name: medvfs-vol
    csi:
      driver: medvfs.colabbio.io
      volumeAttributes:
        configMap: my-vfs-config
```

Current status: **skeleton only** — see [csi-driver.yaml](file:///home/jmolmo/git/colabbio/cb-vfs/deployments/helm/medvfs/templates/csi-driver.yaml).

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Pod CrashLoopBackOff | `/dev/fuse` not found | Ensure node has FUSE module loaded: `modprobe fuse` |
| Mount not visible to other pods | Wrong mount propagation | Set `Bidirectional` on MedVFS, `HostToContainer` on consumer |
| Permission denied | Missing `SYS_ADMIN` capability | Set `privileged: true` or add `CAP_SYS_ADMIN` |
| Low cache hit rate | Cache too small | Increase `vfs.cacheMaxGB` or add local SSD |
| Slow first read | No warmup | Set `vfs.prefetch.strategy: manifest` + increase `warmup_percent` |
| Event stream empty | No streaming adapter | Only DICOM local adapter supports live events |
