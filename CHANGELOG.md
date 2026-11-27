# Changelog

## [0.5.0] - 2026-11-27

### Added
- **Open-Source Public Release**: Licensed under **Apache 2.0** (`Copyright 2026 ColabBio`), with self-contained documentation and the MedVFS preprint (`docs/medvfs_article.md`).
- **Rootless HPC & Cohort ACL Kill-Switch**: Hot FUSE cache eviction on grant revocation, rootless userspace mode for HPC environments, and `nf-core` samplesheet synthesis.
- **Unified Credentials (`CB-SPEC-CRED-022` Phase 1)**: `SecretBundle`, `AuthenticatedRoundTripper`, and per-source fetchers (`k8s://`, `env://`, `file://`).
- **CI, Demo & Public Smoke Suite**: GitHub Actions CI (`.github/workflows/ci.yml`), 1-command self-contained `make demo`, expanded unit tests across `internal/...`, and opt-in `make test-public` (FHIR R4, Orthanc DICOMweb, HTTP Range, unauthenticated `gs://` GCS, and BigQuery public OMOP).

### Fixed
- **Orthanc / DICOMweb JSON Parsing**: Support non-string `Value` entries (e.g., `PersonName` `{"Alphabetic": "..."}` objects) in QIDO-RS responses.

## [0.4.1] - 2026-11-15

### Added / Changed
- Consolidate platform WIP, hygiene updates (`.gitignore`), and multi-repo GitHub workflow alignment.

## [0.3.0] - 2026-08-19

### Added
- **Multi-Node Edge Virtualization & Zero-Egress Validation**:
  - Validated multi-node MedVFS FUSE mounts across simulated transatlantic hospital nodes (`hosp-edge-1` in US, `hosp-edge-2` in EU).
  - Confirmed sub-millisecond local tensor reading for ABMIL bags directly over `/tmp/us-vfs-mount` and `/tmp/eu-vfs-mount` without raw image data transmission over external networks.
  - Hardened local VNA/PACS integration paths for DICOMweb and native WSI files.

## [0.2.0] - 2026-08-17

- **Dual Access Strategies**: Added full support for both dynamic FUSE streaming (`medvfs serve`) and static materialization (`symlink` / `download` for non-privileged K8s pods).
- **Format-Aware Prefetch Engine**: L1/L2/L3 multi-tier block caching for WSI pyramids and DICOM headers.
- **Daemon HTTP & Event API**: Management HTTP server on port `:8090` exposing `/manifest`, `/stats`, and `/events` (SSE).

## [0.1.0] - 2026-07-05

- Initial release.
