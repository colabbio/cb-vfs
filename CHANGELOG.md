# Changelog

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
