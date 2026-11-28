# MedVFS WSI Cache & Prefetch Benchmark

Reproducible benchmark comparing **MedVFS** (`medvfs serve` with 4 MB LRU block cache + 3-level format-aware prefetch) against generic cloud FUSE mounts (`gcsfuse`, `s3fs`) under real **OpenSlide** multi-resolution tile extraction.

## Prerequisites

- Built `medvfs` binary (`make build`)
- Python 3.10+ with `openslide-python` and `Pillow` (`pip install openslide-python pillow`)
- Optional baselines: `gcsfuse` or `s3fs`

## Running the Benchmark

1. Start MedVFS daemon in read-only FUSE mode:

```bash
./medvfs serve -c benchmarks/vfs-tcga-benchmark.yaml --port 8090
```

2. (Optional) Mount the same bucket with `gcsfuse` for baseline comparison:

```bash
mkdir -p /tmp/gcsfuse-bench
gcsfuse --implicit-dirs gdc-tcga-phs000178-open /tmp/gcsfuse-bench
```

3. Run the 2-epoch OpenSlide tile-extraction benchmark (20–50 slides recommended):

```bash
python3 benchmarks/wsi_cache_benchmark.py \
  --mounts medvfs=/tmp/medvfs-bench gcsfuse=/tmp/gcsfuse-bench \
  --max-slides 30 \
  --tiles-per-slide 64 \
  --epochs 2 \
  --stats-url http://127.0.0.1:8090/stats \
  --output-json benchmarks/results.json \
  --output-csv benchmarks/results.csv
```

The script reports median and p95 **Time-to-First-Tile (TTFT)**, **throughput (tiles/s)**, **cache hit rate**, and **bytes fetched** per epoch.
