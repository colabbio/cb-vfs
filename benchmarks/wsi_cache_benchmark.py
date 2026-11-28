#!/usr/bin/env python3
"""Reproducible WSI tile-extraction benchmark for MedVFS vs. generic FUSE mounts.

Simulates real digital pathology workloads (OpenSlide multi-resolution header
inspection + random/strided 256x256 tile reads across a cohort of .svs slides)
over one or more mounted POSIX filesystems (e.g., medvfs, gcsfuse, s3fs).

Usage:
    python3 benchmarks/wsi_cache_benchmark.py \
        --mounts medvfs=/mnt/medvfs gcsfuse=/mnt/gcsfuse \
        --tiles-per-slide 64 \
        --epochs 2 \
        --stats-url http://127.0.0.1:8090/stats \
        --output-json benchmarks/results.json
"""

from __future__ import annotations

import argparse
import csv
import json
import random
import statistics
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any


@dataclass
class EpochResult:
    mount_name: str
    epoch: int
    slides_count: int
    total_slide_bytes: int
    tiles_extracted: int
    ttft_median_ms: float
    ttft_p95_ms: float
    total_duration_s: float
    throughput_tiles_per_s: float
    medvfs_hits_delta: int | None = None
    medvfs_misses_delta: int | None = None
    medvfs_hit_rate_pct: float | None = None
    medvfs_bytes_fetched_delta: int | None = None


def fetch_medvfs_stats(stats_url: str) -> dict[str, Any] | None:
    """Query the MedVFS /stats endpoint; return None if unreachable."""
    try:
        with urllib.request.urlopen(stats_url, timeout=3.0) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        return None


def discover_slides(mount_dir: Path, max_slides: int | None) -> list[Path]:
    """Find .svs/.tiff slides recursively under mount_dir in deterministic order."""
    slides = sorted(
        p for p in mount_dir.rglob("*")
        if p.is_file() and p.suffix.lower() in {".svs", ".tif", ".tiff"}
    )
    if max_slides is not None:
        slides = slides[:max_slides]
    return slides


def extract_tiles_openslide(
    slide_path: Path,
    tiles_per_slide: int,
    tile_size: int,
    rng: random.Random,
) -> tuple[float, int]:
    """Open a slide with OpenSlide and read tiles_per_slide regions.

    Returns (time_to_first_tile_ms, tiles_read).
    """
    import openslide  # Lazy import so --help works without OpenSlide installed.

    t0 = time.perf_counter()
    slide = openslide.OpenSlide(str(slide_path))
    try:
        # Prefer level 1 or 2 if available to mimic multi-scale pyramid access,
        # while still exercising TIFF IFD + region decoding.
        level = min(1, slide.level_count - 1)
        width, height = slide.level_dimensions[level]
        downsample = slide.level_downsamples[level]

        max_x = max(1, width - tile_size)
        max_y = max(1, height - tile_size)

        ttft_ms = 0.0
        for idx in range(tiles_per_slide):
            lx = rng.randint(0, max_x)
            ly = rng.randint(0, max_y)
            # OpenSlide read_region expects level-0 coordinates.
            x0 = int(lx * downsample)
            y0 = int(ly * downsample)
            _ = slide.read_region((x0, y0), level, (tile_size, tile_size)).convert("RGB")
            if idx == 0:
                ttft_ms = (time.perf_counter() - t0) * 1000.0
        return ttft_ms, tiles_per_slide
    finally:
        slide.close()


def benchmark_mount(
    mount_name: str,
    mount_dir: Path,
    tiles_per_slide: int,
    tile_size: int,
    epochs: int,
    max_slides: int | None,
    seed: int,
    stats_url: str | None,
) -> list[EpochResult]:
    """Run multi-epoch WSI tile extraction on a single POSIX mount."""
    slides = discover_slides(mount_dir, max_slides)
    if not slides:
        raise FileNotFoundError(f"No .svs/.tiff slides found under {mount_dir}")

    total_slide_bytes = sum(s.stat().st_size for s in slides)
    results: list[EpochResult] = []

    for epoch in range(1, epochs + 1):
        rng = random.Random(seed + epoch)
        stats_before = fetch_medvfs_stats(stats_url) if stats_url else None

        ttfts_ms: list[float] = []
        total_tiles = 0
        t_epoch_start = time.perf_counter()

        for slide in slides:
            ttft_ms, n_tiles = extract_tiles_openslide(
                slide, tiles_per_slide=tiles_per_slide, tile_size=tile_size, rng=rng
            )
            ttfts_ms.append(ttft_ms)
            total_tiles += n_tiles

        duration_s = time.perf_counter() - t_epoch_start
        stats_after = fetch_medvfs_stats(stats_url) if stats_url else None

        hits_delta = misses_delta = bytes_delta = None
        hit_rate_pct = None
        if stats_before and stats_after:
            c_before = stats_before.get("cache", {})
            c_after = stats_after.get("cache", {})
            h0 = int(c_before.get("HitCount", c_before.get("hit_count", 0)))
            h1 = int(c_after.get("HitCount", c_after.get("hit_count", 0)))
            m0 = int(c_before.get("MissCount", c_before.get("miss_count", 0)))
            m1 = int(c_after.get("MissCount", c_after.get("miss_count", 0)))
            b0 = int(c_before.get("BytesFetched", c_before.get("bytes_fetched", 0)))
            b1 = int(c_after.get("BytesFetched", c_after.get("bytes_fetched", 0)))

            hits_delta = max(0, h1 - h0)
            misses_delta = max(0, m1 - m0)
            bytes_delta = max(0, b1 - b0)
            total_ops = hits_delta + misses_delta
            hit_rate_pct = (hits_delta / total_ops * 100.0) if total_ops > 0 else 0.0

        sorted_ttft = sorted(ttfts_ms)
        p95_idx = min(len(sorted_ttft) - 1, int(0.95 * len(sorted_ttft)))

        results.append(
            EpochResult(
                mount_name=mount_name,
                epoch=epoch,
                slides_count=len(slides),
                total_slide_bytes=total_slide_bytes,
                tiles_extracted=total_tiles,
                ttft_median_ms=round(statistics.median(ttfts_ms), 2),
                ttft_p95_ms=round(sorted_ttft[p95_idx], 2),
                total_duration_s=round(duration_s, 3),
                throughput_tiles_per_s=round(total_tiles / max(duration_s, 1e-6), 2),
                medvfs_hits_delta=hits_delta,
                medvfs_misses_delta=misses_delta,
                medvfs_hit_rate_pct=round(hit_rate_pct, 2) if hit_rate_pct is not None else None,
                medvfs_bytes_fetched_delta=bytes_delta,
            )
        )

    return results


def parse_mounts(raw_mounts: list[str]) -> list[tuple[str, Path]]:
    """Parse name=/path pairs into validated (name, Path) tuples."""
    parsed: list[tuple[str, Path]] = []
    for item in raw_mounts:
        if "=" not in item:
            raise ValueError(f"Invalid --mounts entry '{item}'; expected name=/path/to/mount")
        name, path_str = item.split("=", 1)
        parsed.append((name.strip(), Path(path_str.strip())))
    return parsed


def print_summary(results: list[EpochResult]) -> None:
    """Print a Markdown-compatible summary table to stdout."""
    print(
        "\n| Mount | Epoch | Slides | Tiles | TTFT Median (ms) | TTFT p95 (ms) | "
        "Duration (s) | Tiles/s | Hit Rate (%) | Fetched (MB) |"
    )
    print("| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |")
    for r in results:
        hr = f"{r.medvfs_hit_rate_pct:.1f}%" if r.medvfs_hit_rate_pct is not None else "N/A"
        fetched_mb = (
            f"{r.medvfs_bytes_fetched_delta / (1024 * 1024):.1f}"
            if r.medvfs_bytes_fetched_delta is not None
            else "N/A"
        )
        print(
            f"| {r.mount_name} | {r.epoch} | {r.slides_count} | {r.tiles_extracted} | "
            f"{r.ttft_median_ms} | {r.ttft_p95_ms} | {r.total_duration_s} | "
            f"{r.throughput_tiles_per_s} | {hr} | {fetched_mb} |"
        )


def main() -> int:
    parser = argparse.ArgumentParser(description="WSI OpenSlide tile-extraction benchmark")
    parser.add_argument(
        "--mounts",
        nargs="+",
        required=True,
        help="Mount points to benchmark as name=/path (e.g. medvfs=/mnt/medvfs gcsfuse=/mnt/gcsfuse)",
    )
    parser.add_argument("--tiles-per-slide", type=int, default=64, help="Tiles to sample per slide")
    parser.add_argument("--tile-size", type=int, default=256, help="Tile width/height in pixels")
    parser.add_argument("--epochs", type=int, default=2, help="Number of epochs (1=cold, >=2=warm)")
    parser.add_argument("--max-slides", type=int, default=None, help="Limit number of slides")
    parser.add_argument("--seed", type=int, default=42, help="Deterministic RNG seed")
    parser.add_argument(
        "--stats-url",
        default="http://127.0.0.1:8090/stats",
        help="MedVFS telemetry /stats URL (used for mount named 'medvfs')",
    )
    parser.add_argument("--output-json", type=Path, default=None, help="Write JSON results")
    parser.add_argument("--output-csv", type=Path, default=None, help="Write CSV results")
    args = parser.parse_args()

    try:
        mounts = parse_mounts(args.mounts)
    except ValueError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 2

    all_results: list[EpochResult] = []
    for name, path in mounts:
        stats_url = args.stats_url if "medvfs" in name.lower() else None
        results = benchmark_mount(
            mount_name=name,
            mount_dir=path,
            tiles_per_slide=args.tiles_per_slide,
            tile_size=args.tile_size,
            epochs=args.epochs,
            max_slides=args.max_slides,
            seed=args.seed,
            stats_url=stats_url,
        )
        all_results.extend(results)

    print_summary(all_results)

    if args.output_json:
        args.output_json.parent.mkdir(parents=True, exist_ok=True)
        args.output_json.write_text(json.dumps([asdict(r) for r in all_results], indent=2))

    if args.output_csv and all_results:
        args.output_csv.parent.mkdir(parents=True, exist_ok=True)
        with args.output_csv.open("w", newline="") as fh:
            writer = csv.DictWriter(fh, fieldnames=list(asdict(all_results[0]).keys()))
            writer.writeheader()
            for r in all_results:
                writer.writerow(asdict(r))

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
