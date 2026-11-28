# MedVFS: A Declarative Medical Virtual File System for Federated AI in Biology, Clinical Informatics, and Open Science

**David Olmo Marchal**¹\*, **Lorenzo Olmo Marchal**¹, and **Jose Maria Olmo Millan**¹  

¹ ColabBio Project (`https://colabbio.org`)  
\* Correspondence: `dolmomar@colabbio.org` (`lolmo@colabbio.org`, `jmolmo@colabbio.org`)  

**bioRxiv Category:** Bioinformatics  

---

## Abstract

**Motivation:** Computational biology and clinical artificial intelligence increasingly rely on multimodal cohorts spanning gigapixel whole-slide imaging (WSI), high-throughput genomics, electronic health records (OMOP CDM, HL7 FHIR R4), and hospital PACS/microscopy archives (DICOMweb, OMERO). However, standard bioinformatics orchestrators (e.g., Nextflow) and deep learning frameworks (e.g., PyTorch, OpenSlide) expect local POSIX filesystems, whereas clinical data reside behind heterogeneous, strictly regulated APIs and databases. Conventional extract-transform-load (ETL) pipelines copy sensitive patient data into centralized data lakes, introducing storage duplication, synchronization overhead, and regulatory risk under frameworks such as the General Data Protection Regulation (GDPR).

**Results:** We present **MedVFS** (*Medical Virtual File System*), the Go-based data-plane orchestrator of the **ColabBio** (`colabbio.org`) platform. Guided by the principle *"The data doesn't move; the model goes to the data,"* MedVFS exposes heterogeneous clinical and biological sources as a unified, read-only POSIX filesystem via FUSE (`medvfs serve`) or as deterministic batch snapshots (`medvfs setup`), governed by a single declarative `vfs.yaml` manifest. MedVFS introduces a 4 MB block-based LRU cache, HTTP Range streaming, and a 3-level format-aware prefetch engine (TIFF IFDs, DICOM `PixelData`, BAM headers) alongside an ML Dataset Engine for automated patient linking, deterministic splits, and SHA-256 versioning. In live evaluation on a 25-slide remote S3 TCGA-BRCA cohort (9.34 GB) using OpenSlide tile extraction (800 tiles/epoch), MedVFS achieved a **95.9% cold-cache hit rate** while fetching only **19.1%** of the cohort (**80.9% bandwidth reduction**), rising to **99.97% hit rate** with a **125× Time-to-First-Tile speedup** in Epoch 2. In an end-to-end digital pathology case study (**SlideLab** + Nextflow 25.04.6 + ABMIL), MedVFS enabled on-the-fly streaming that eliminated full-slide local staging and 5,000–20,000 intermediate tile files per slide (**99.6% storage reduction**), yielded a **3×–4× pipeline speedup**, and—paired with SlideLab's lightweight `slidelab:cpu` container—achieved **<3 s Kubernetes Pod cold-starts**.

**Availability:** Source code is developed at `https://github.com/colabbio/cb-vfs` (Go module `github.com/colabbio/medvfs`).

---

## 1. Introduction

Modern biomedical discovery sits at the intersection of **AI in Biology**—characterized by multi-gigabyte WSI pathology slides (`.svs`, `.tiff`), aligned sequencing reads (`.bam`, `.cram`), and variant callsets (`.vcf`)—and **Clinical Informatics**, anchored in longitudinal electronic health records (OMOP Common Data Model, HL7 FHIR R4) and clinical imaging repositories (Orthanc DICOMweb, OMERO) [1–4]. Bridging these two domains is essential for precision oncology, biomarker discovery, and real-world evidence (RWE) studies.

In practice, however, data engineering remains a major barrier. Bioinformaticians and machine learning (ML) engineers build reproducible pipelines using Nextflow/`nf-core` [5], OpenSlide [6], and PyTorch, all of which assume POSIX file semantics. Hospital IT environments, conversely, enforce strict perimeter security, data minimization, and access auditing. Centralizing raw Protected Health Information (PHI) into external cloud buckets creates friction with **Data Protection Officers (DPOs)** and **Chief Security Officers (CSOs)** under **GDPR** [11] and the **EU AI Act** [12], while duplicating terabytes of imaging and genomic data saturates storage and network bandwidth.

**Related Work and Positioning:** Existing biomedical data platforms fall into three categories, none of which simultaneously satisfy open-source, on-edge, and multimodal ML requirements (**Table 1**). Centralized Real-World Evidence (RWE) platforms (e.g., Flatiron Health, IQVIA, Aetion, TriNetX) and cloud-hosted health lakes (e.g., BigQuery Health, AWS HealthLake) require hospitals to export patient data off-premise, creating severe GDPR compliance friction and vendor lock-in. Proprietary on-edge platforms (e.g., Palantir Foundry, Rhino Health) preserve local data residency but rely on closed-source stacks with high licensing costs and limited native support for open bioinformatics standards. Finally, open-source federated query networks such as OHDSI/ATLAS [1] and DataSHIELD [8] keep data at the hospital node, but are restricted to distributed SQL or R statistical queries—they cannot orchestrate containerized deep learning workloads or Nextflow pipelines across gigapixel WSI, DICOM, and genomic binaries. Similarly, generic cloud FUSE mounts (`gcsfuse` [9], `s3fs` [10]) lack biomedical format awareness (TIFF IFDs, DICOM tags, BAM headers) and cannot bridge relational clinical databases (OMOP/FHIR) alongside imaging files. **ColabBio + MedVFS** fills this empty quadrant: an **open-source, on-edge, Kubernetes-native** platform supporting multimodal ML containers.

| Platform Category | Representative Systems | Data Residency | Modalities | Arbitrary ML / Nextflow Containers | Open Source |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Centralized RWE / Cloud Lakes** | Flatiron, IQVIA, TriNetX, AWS HealthLake | Off-premise (Centralized) | Primarily EHR / Claims | Limited / Cloud-locked | No (Proprietary) |
| **Proprietary On-Edge** | Palantir Foundry, Rhino Health | On-premise (Hospital) | EHR / Imaging (varies) | Vendor-managed sandboxes | No (Proprietary) |
| **Open-Source Federated Query** | OHDSI/ATLAS [1], DataSHIELD | On-premise (Hospital) | OMOP CDM / Tabular | No (SQL / R queries only) | Yes |
| **Open-Source On-Edge Multimodal** | **ColabBio + MedVFS** | **On-premise (Hospital)** | **OMOP + FHIR + WSI + DICOM + Genomics** | **Yes (K8s Sandbox + Nextflow)** | **Yes** |

**Table 1.** Architectural comparison of biomedical and clinical study platforms across data residency, modality coverage, and containerized ML support.

---

## 2. Methods and System Architecture

### 2.1 The `colabbio.org` Ecosystem and MedVFS Data-Plane Role
**ColabBio** (`colabbio.org`) is an open, modular clinical data and biomedical AI infrastructure initiative designed to unify hospital IT environments and reproducible computational science. Rather than requiring centralized data lakes, ColabBio integrates declarative specifications (`cb-specs`), Kubernetes/MicroK8s orchestration (`cb-devops`), Nextflow workflow execution, and **MedVFS** (`cb-vfs`). Within this architecture, MedVFS serves as the **data-plane orchestrator**, abstracting heterogeneous storage protocols so that downstream biological and clinical AI workloads consume data uniformly.

### 2.2 Dual-Mode Execution: `setup` and `serve`
Implemented in Go (`go 1.26.4`) as a single static binary driven by `vfs.yaml`, MedVFS supports two operational modes (**Figure 1**):

1. **`medvfs setup` (Batch Materialization):** Extracts and transforms configured sources into flat files on local disk or Persistent Volume Claims (PVCs). This mode produces immutable, SHA-256-verified snapshots suited for Nextflow inputs and regulatory-grade studies requiring frozen artifacts (e.g., ICH E6, 21 CFR Part 11).
2. **`medvfs serve` (Persistent FUSE Daemon):** Mounts a read-only virtual filesystem (default `/data/vfs`) backed by remote sources while exposing an HTTP/Server-Sent Events (SSE) control and telemetry API on port `:8090` (`/manifest`, `/dataset`, `/stats`, `/events` via `fsnotify`, and `/proxy/{path}` as an HTTP fallback for FUSE-less containers).

![Figure 1. Dual-mode architecture of MedVFS.](figures/figure1_architecture.svg)

**Figure 1.** Dual-mode architecture of MedVFS (`setup` batch materialization vs. `serve` persistent FUSE streaming). Vector sources: `figures/figure1_architecture.mmd` / `.pdf`.

### 2.3 Block-Based FUSE and 3-Level Format-Aware Prefetch
In `serve` mode, remote objects are partitioned into **4 MB blocks** managed by a `BlockManager` with Least Recently Used (LRU) eviction and an `HTTP Range Fetcher` supporting Google Cloud Storage (GCS), Amazon S3, and NFS. To minimize latency during random-access reads typical of biomedical formats, MedVFS implements a 3-level prefetch engine:
- **L1 (Sequential Read-Ahead):** Pre-fetches blocks $N+1 \dots N+3$ during sequential scans.
- **L2 (Format-Aware Prefetch):** Inspects file-open and block-load events to pre-cache structural metadata—specifically **TIFF IFDs** (required for `.svs` WSI pyramids), **DICOM `PixelData` tags**, and **BAM headers**.
- **L3 (Manifest Warmup):** Proactively warms headers of the smallest files at daemon startup to accelerate initial directory and metadata discovery.

### 2.4 Source Adapters, ML Dataset Engine, and Governance
MedVFS provides seven pluggable adapters: `omop` (PostgreSQL SQL-to-CSV), `fhir` (FHIR R4 REST-to-CSV), `omero` (OMERO.web JSON API), `orthanc` (DICOMweb QIDO/WADO-RS), `dicom` (local symlink trees), `genomics` (BAM/CRAM/VCF with index validation), and `files` (1:1 static mapping with glob filters). A **FHIR/OMOP Bridge** injects flattened clinical tables alongside imaging files, while the **ML Dataset Engine** performs cross-source patient linking, deterministic `train`/`val`/`test` splitting, and SHA-256 manifest generation. A **Writable Output Sync** module periodically synchronizes model checkpoints, predictions, and logs back to GCS or S3.

From a governance perspective, MedVFS supports **GDPR** compliance by design: `serve` mode streams only requested byte ranges without copying raw PHI outside the hospital perimeter. **DPOs** gain auditable data provenance through `vfs.yaml` contracts and SHA-256 manifests, while **CSOs** benefit from read-only POSIX mounts, zero temporary PHI dumps, and externalized authentication (**SMART-on-FHIR**, Bearer, Basic, and `k8s://`, `env://`, `file://` secret references). For Kubernetes orchestration, MedVFS ships with a Helm chart supporting `DaemonSet`, `Sidecar`, and `Standalone` modes.

### 2.5 Federated Hospital Networks, Multi-Center Studies, and Open Science
Unlike centralized study platforms that force participating hospitals to upload raw patient cohorts to a third-party cloud, ColabBio and MedVFS execute multi-center studies **at the hospital edge** (**Figure 2**). A consortium or study coordinator distributes a declarative study package—comprising a `vfs.yaml` data contract (with environment overlays for `dev` vs. `prod`) and a containerized Nextflow/ML pipeline. At each participating hospital node:
1. **Local Data Binding:** MedVFS binds to the hospital's sovereign systems (OMOP CDM, FHIR R4, Orthanc PACS, OMERO) using local credentials (`k8s://` or SMART-on-FHIR).
2. **Sandboxed Execution:** The study container runs inside a Kubernetes sandbox with a **read-only MedVFS mount** and strict **`NetworkPolicy` isolation** (preventing outbound exfiltration of raw PHI).
3. **Auditability & EU AI Act Traceability:** MedVFS generates a deterministic `manifest.json` with SHA-256 checksums for every cohort snapshot, providing end-to-end training-data lineage aligned with **GDPR** and **EU AI Act** high-risk medical AI requirements.
4. **Federated Aggregation:** Only non-PHI outputs—such as trained model weights, validation metrics, or summary statistics—leave the hospital perimeter to join the consortium's model registry alongside reproducible benchmarks run on public open-data lakes (e.g., TCGA or IDC on GCS/S3).

![Figure 2. Federated hospital network topology.](figures/figure2_federated_topology.svg)

**Figure 2.** Federated hospital network topology for multi-center clinical and biological studies using MedVFS and ColabBio, contrasting with centralized data-lake platforms by keeping all raw PHI inside sovereign hospital perimeters. Vector sources: `figures/figure2_federated_topology.mmd` / `.pdf`.

---

## 3. Results

### 3.1 Live FUSE Mounting and Cache Efficiency
We evaluated `medvfs serve` on a seed cohort (`tcga-brca-seed-vfs`) comprising public TCGA-BRCA whole-slide images (`TCGA-A2-A0T0-01Z-00-DX1.svs`, 1,559,068,211 bytes [1.55 GB]; `TCGA-A2-A0T2-01Z-00-DX1.svs`, 273,678,336 bytes) and `.h5` feature files. Standard POSIX utilities (`ls`, `file`) inspected the virtualized `/tmp/medvfs` mount with zero-copy TIFF header resolution. Live telemetry queried from the `:8090/stats` endpoint recorded **116 cache hits versus 8 misses**, corresponding to a **93.5% cache hit rate** (~32.3 MB fetched/cached) without downloading the full gigapixel slides.

To evaluate the 4 MB LRU block cache and 3-level format-aware prefetch engine under realistic deep-learning I/O load, we ran a 2-epoch OpenSlide (v1.4.7) multi-resolution tile-extraction benchmark (`benchmarks/wsi_cache_benchmark.py`) across two cohorts:
1. **25-slide Remote S3 Cohort (`s3://tcga-2-open`, 9.34 GB / 8.70 GiB):** 25 TCGA-BRCA `.svs` slides mounted remotely over WAN via HTTP Range (`allow_other: false`), extracting 32 random 256×256 RGB tiles at pyramid level 1 per slide (800 tiles/epoch; 1,600 tiles total).
2. **3-slide Local Baseline Cohort (5.24 GB / 4.88 GiB):** 3 TCGA-BRCA `.svs` slides comparing `medvfs serve` against direct local POSIX disk access (128 tiles/slide; 384 tiles/epoch).

**Table 2** summarizes the measured cache efficiency, Time-to-First-Tile (TTFT), and network transfer savings across both cohorts:

| Cohort & Mount | Epoch | Slides | Tiles | TTFT Median (ms) | TTFT p95 (ms) | Duration (s) | Throughput (tiles/s) | Cache Hit Rate | Bytes Fetched |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Remote S3 (`tcga-2-open`) — MedVFS** | 1 (Cold) | 25 | 800 | 5,530.09 | 911,218.76 | 5,097.30 | 0.16 | **95.9%** (10,345 / 444) | **1,704.4 MiB** (19.1% of 8.70 GiB) |
| **Remote S3 (`tcga-2-open`) — MedVFS** | 2 (Warm) | 25 | 800 | **44.08** | **80.97** | **1,039.94** | **0.77** | **100.0%** (10,481 / 3) | **20.0 MiB** |
| **Local POSIX — MedVFS (FUSE)** | 1 (Cold) | 3 | 384 | 557.82 | 1,090.21 | 9.59 | 40.05 | **94.5%** (3,283 / 190) | **755.4 MiB** (15.1% of 4.88 GiB) |
| **Local POSIX — MedVFS (FUSE)** | 2 (Warm) | 3 | 384 | **26.56** | **73.36** | **4.52** | **84.95** | **100.0%** (3,328 / 0) | **0.0 MiB** |
| Local POSIX — Raw Disk | 1 | 3 | 384 | 18.85 | 33.24 | 3.67 | 104.70 | N/A | N/A |
| Local POSIX — Raw Disk | 2 | 3 | 384 | 17.18 | 35.75 | 3.51 | 109.47 | N/A | N/A |

**Table 2.** Multi-epoch OpenSlide tile-extraction benchmark across a 25-slide remote S3 TCGA-BRCA cohort (9.34 GB) and a 3-slide local baseline (5.24 GB). On the 25-slide remote S3 cohort, MedVFS achieved a **95.9% cache hit rate** in Epoch 1 while fetching only **1,704.4 MiB** (an **80.9% bandwidth reduction** vs. full-cohort download). In Epoch 2, warm-cache hits reached **99.97%** (`10,481 / 3`), reducing median TTFT by **125×** (5,530.09 ms → 44.08 ms) and total epoch duration by **4.9×**. On the local baseline, warm-cache MedVFS reached 77.6% of raw local disk throughput. The reproducible harness and GCS cohort manifest (`benchmarks/vfs-tcga-benchmark.yaml`) also support comparisons against `gcsfuse` [9] and `s3fs` [10].

### 3.2 End-to-End Case Study: SlideLab On-The-Fly WSI Streaming and ABMIL
We integrated MedVFS into **SlideLab**, a digital pathology feature-extraction and Attention-Based Multiple Instance Learning (ABMIL) [7] pipeline orchestrated in **Nextflow 25.04.6 DSL2** (`cb-slidelab-abmil/pipelines/main.nf`). The pipeline executes five stages: `SLIDELAB_EXTRACT`, `WRITE_OMERO_ANNOTATIONS`, `ABMIL_TRAIN`, `MLFLOW_REGISTER`, and `TRITON_DEPLOY`.

Using MedVFS POSIX mounts, SlideLab's `StreamingTileDataset` employs lazy OpenSlide C-layer loading to stream tissue tiles directly into RAM/VRAM, apply quality-control (QC) filters (tissue, blur, pen), and encode ResNet50/UNI embeddings atomically into `features.h5`, while persisting QC and PAC metrics back to OMERO as `MapAnnotation` records (`colabbio.ai.slidelab.qc_and_mil`). **Table 3** distinguishes improvements directly attributable to **MedVFS** (eliminating full-slide staging and intermediate disk tiles via HTTP Range streaming) from runtime optimizations in **SlideLab** (container footprint).

| Pipeline Metric | Traditional Disk-Based Workflow | MedVFS + SlideLab On-The-Fly | Observed Improvement | Primary Attribution |
| :--- | :--- | :--- | :--- | :--- |
| **Full `.svs` node staging** | Full slide pre-copied (~0.3–1.6 GB / WSI) | **0 bytes pre-staged** (HTTP Range on demand) | **Zero-copy mount** | **MedVFS** |
| **Intermediate tile files / WSI** | 5,000–20,000 PNG/JPEG files | **0 files** (in-memory tensors) | **100% eliminated** | **MedVFS + SlideLab** |
| **Disk volume written / WSI** | ~2.5–5.0 GB | **~10–20 MB** (`features.h5`) | **99.6% reduction** | **MedVFS + SlideLab** |
| **Pipeline throughput** | Write-then-read I/O bottleneck | Single-pass memory streaming | **3×–4× speedup** | **MedVFS + SlideLab** |
| **K8s Pod cold-start** | Monolithic GPU image (~18 GB) | `slidelab:cpu` (~1.2 GB) | **< 3 seconds** | **SlideLab Container** |

**Table 3.** End-to-end comparison of traditional disk-staged pathology workflows versus MedVFS on-the-fly streaming paired with SlideLab, separating data-plane (MedVFS) and container-runtime (SlideLab) contributions.

By eliminating full-slide node staging and millions of small temporary tile files, the MedVFS–SlideLab integration removes PVC IOPS saturation and filesystem inode locking across multi-worker PyTorch runs.

---

## 4. Discussion

MedVFS demonstrates that unifying **AI in Biology** and **Clinical Informatics** does not require centralizing sensitive patient data or refactoring established bioinformatics software. By pairing format-aware FUSE block streaming with declarative `vfs.yaml` specifications, MedVFS satisfies the operational needs of bioinformaticians while addressing the legal and security mandates of hospital DPOs and CSOs under GDPR.

A key architectural takeaway is that high-throughput multimodal streaming does not require heavy middleware or patches to downstream bioinformatics tools. By reducing the kernel–userspace contract in `hanwen/go-fuse/v2` to read-only POSIX `read(offset, size)` calls backed by aligned 4 MB HTTP Range blocks (`BlockManager`), MedVFS exploits the sparse, pyramidal layout of biomedical formats (tiled TIFF/SVS pyramids and indexed BAMs). In practice, a single OpenSlide region query issues dozens of small, spatially localized POSIX reads that hit the same cached 4 MB block—explaining both the **80.9% network bandwidth reduction** on cold passes and the **95.9%–99.97% cache hit rates** achieved within a single static Go binary.

Current limitations include reliance on privileged FUSE mounts in containerized environments (mitigated via the `/proxy/{path}` HTTP fallback) and static node configuration. Future work will complete a production Kubernetes CSI driver for dynamic multi-tenant volume provisioning, introduce post-materialization Nextflow execution hooks, and expand public health system ingestion adapters to further lower barriers for international open-science consortia.

---

## 5. Code and Data Availability

- **Source Code & License:** The MedVFS engine (`cb-vfs`, Go module `github.com/colabbio/medvfs`) is publicly available under the **Apache License 2.0** (`https://github.com/colabbio/cb-vfs`), alongside ColabBio platform specifications (`cb-specs`, `cb-devops`, `cb-slidelab-abmil`) under `https://github.com/colabbio`.
- **Reproducible Manifests & Benchmarks:** Example `vfs.yaml` configurations are provided in `deployments/examples/` (e.g., `deployments/examples/vfs-dev.yaml`, `deployments/examples/vfs-specs/vfs-omero-wsi.yaml`), and the OpenSlide multi-epoch WSI cache benchmark harness is available at `benchmarks/wsi_cache_benchmark.py` (`benchmarks/vfs-tcga-benchmark.yaml`).
- **Benchmark Data:** Live FUSE caching benchmarks used publicly available TCGA-BRCA whole-slide images (`TCGA-A2-A0T0-01Z-00-DX1`, `TCGA-A2-A0T2-01Z-00-DX1`) from The Cancer Genome Atlas (TCGA) / Genomic Data Commons (`https://portal.gdc.cancer.gov`).

---

## Author Contributions

MedVFS (`cb-vfs`) is developed as a core component of the **ColabBio** (`colabbio.org`) project. **Jose Maria Olmo Millan** is the Architect of the ColabBio platform, conceiving its federated data-plane model, declarative multi-tenant architecture, and regulatory-by-design approach. **David Olmo Marchal** is the primary implementer of MedVFS, leading the development of the Go FUSE daemon, 3-level prefetch engine, clinical/genomic source adapters, and Kubernetes deployment tooling. **Lorenzo Olmo Marchal** applied and validated ColabBio and MedVFS in his Master of Science (MS) thesis project on **SlideLab**, integrating on-the-fly WSI streaming with ABMIL prediction models.

---

## References

1. Hripcsak G, et al. Observational Health Data Sciences and Informatics (OHDSI): Opportunities for Observational Researchers. *Stud Health Technol Inform*. 2015;216:574–578.
2. HL7 International. Fast Healthcare Interoperability Resources (FHIR) R4 Specification. `https://hl7.org/fhir/R4/`.
3. Allan C, et al. OMERO: flexible, model-driven data management for experimental biology. *Nat Methods*. 2012;9(3):245–253.
4. Jodogne S. The Orthanc Ecosystem for Medical Imaging. *J Digit Imaging*. 2018;31(3):341–352.
5. Di Tommaso P, et al. Nextflow enables reproducible computational workflows. *Nat Biotechnol*. 2017;35(4):316–319.
6. Goode A, et al. OpenSlide: A vendor-neutral software foundation for digital pathology. *J Pathol Inform*. 2013;4:27.
7. Ilse M, Tomczak J, Welling M. Attention-based Deep Multiple Instance Learning. *Proc ICML*. 2018;80:2127–2136.
8. Gaye A, et al. DataSHIELD: taking the analysis to the data, not the data to the analysis. *Int J Epidemiol*. 2014;43(6):1929–1944.
9. Google Cloud. Cloud Storage FUSE (`gcsfuse`). `https://github.com/GoogleCloudPlatform/gcsfuse`.
10. `s3fs-fuse`: FUSE-based file system backed by Amazon S3. `https://github.com/s3fs-fuse/s3fs-fuse`.
11. European Parliament and Council. Regulation (EU) 2016/679 (General Data Protection Regulation, GDPR). *Official Journal of the European Union*. 2016;L119:1–88.
12. European Parliament and Council. Regulation (EU) 2024/1689 (Artificial Intelligence Act, EU AI Act). *Official Journal of the European Union*. 2024.
