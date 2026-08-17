# Changelog

## [0.2.0] - 2026-08-17

- **Dual Access Strategies**: Added full support for both dynamic FUSE streaming (`medvfs serve`) and static materialization (`symlink` / `download` for non-privileged K8s pods).
- **Format-Aware Prefetch Engine**: L1/L2/L3 multi-tier block caching for WSI pyramids and DICOM headers.
- **Daemon HTTP & Event API**: Management HTTP server on port `:8090` exposing `/manifest`, `/stats`, and `/events` (SSE).

## [0.1.0] - 2026-07-05

- Initial release.
