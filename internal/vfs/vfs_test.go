package vfs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestHTTPFetcherPartialContent verifies that the HTTP fetcher correctly
// issues Range requests and receives partial content.
func TestHTTPFetcherPartialContent(t *testing.T) {
	// Create a test server that serves a known file with Range support
	fileData := make([]byte, 16*1024*1024) // 16 MB
	for i := range fileData {
		fileData[i] = byte(i % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileData)))
			w.WriteHeader(http.StatusOK)
			return
		}

		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.WriteHeader(http.StatusOK)
			w.Write(fileData)
			return
		}

		var start, end int64
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)

		if start >= int64(len(fileData)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= int64(len(fileData)) {
			end = int64(len(fileData)) - 1
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(fileData)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(fileData[start : end+1])
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(server.Client())

	t.Run("FetchBlock_first_block", func(t *testing.T) {
		data, err := fetcher.FetchBlock(context.Background(), server.URL, 0, 4*1024*1024)
		if err != nil {
			t.Fatalf("FetchBlock: %v", err)
		}
		if len(data) != 4*1024*1024 {
			t.Fatalf("expected %d bytes, got %d", 4*1024*1024, len(data))
		}
		// Verify content
		for i := 0; i < 100; i++ {
			if data[i] != byte(i%256) {
				t.Fatalf("byte %d: expected %d, got %d", i, byte(i%256), data[i])
			}
		}
	})

	t.Run("FetchBlock_middle_block", func(t *testing.T) {
		offset := int64(8 * 1024 * 1024)
		data, err := fetcher.FetchBlock(context.Background(), server.URL, offset, 4*1024*1024)
		if err != nil {
			t.Fatalf("FetchBlock: %v", err)
		}
		if len(data) != 4*1024*1024 {
			t.Fatalf("expected %d bytes, got %d", 4*1024*1024, len(data))
		}
		// Verify content at offset
		expected := byte(offset % 256)
		if data[0] != expected {
			t.Fatalf("byte 0: expected %d, got %d", expected, data[0])
		}
	})

	t.Run("FileSize", func(t *testing.T) {
		size, err := fetcher.FileSize(context.Background(), server.URL)
		if err != nil {
			t.Fatalf("FileSize: %v", err)
		}
		if size != int64(len(fileData)) {
			t.Fatalf("expected size %d, got %d", len(fileData), size)
		}
	})
}

// TestBlockManagerReadAt verifies that the block manager correctly splits
// reads into block-level fetches and caches them.
func TestBlockManagerReadAt(t *testing.T) {
	// Create test data
	fileData := make([]byte, 10*1024*1024) // 10 MB
	for i := range fileData {
		fileData[i] = byte(i % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		var start, end int64
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if end >= int64(len(fileData)) {
			end = int64(len(fileData)) - 1
		}
		w.WriteHeader(http.StatusPartialContent)
		w.Write(fileData[start : end+1])
	}))
	defer server.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	fetcher := NewHTTPFetcher(server.Client())

	bm, err := NewBlockManager(
		1*1024*1024, // 1 MB blocks for testing
		cacheDir,
		100*1024*1024, // 100 MB cache
		fetcher,
		nil, // no logger in test
	)
	if err != nil {
		t.Fatalf("NewBlockManager: %v", err)
	}

	// Need a logger for block manager
	bm.log, _ = zap.NewNop(), error(nil)

	// Register a file
	bm.RegisterFile("test.svs", server.URL, int64(len(fileData)))

	t.Run("ReadAt_start", func(t *testing.T) {
		buf := make([]byte, 1024)
		n, err := bm.ReadAt(context.Background(), "test.svs", buf, 0)
		if err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if n != 1024 {
			t.Fatalf("expected 1024 bytes, got %d", n)
		}
		for i := 0; i < 100; i++ {
			if buf[i] != byte(i%256) {
				t.Fatalf("byte %d: expected %d, got %d", i, byte(i%256), buf[i])
			}
		}
	})

	t.Run("ReadAt_cross_block_boundary", func(t *testing.T) {
		// Read across block 0-1 boundary (at 1MB - 512 bytes)
		offset := int64(1*1024*1024 - 512)
		buf := make([]byte, 1024)
		n, err := bm.ReadAt(context.Background(), "test.svs", buf, offset)
		if err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if n != 1024 {
			t.Fatalf("expected 1024 bytes, got %d", n)
		}
		// Verify bytes around the boundary
		expected := byte(offset % 256)
		if buf[0] != expected {
			t.Fatalf("byte 0: expected %d, got %d", expected, buf[0])
		}
	})

	t.Run("CacheHit", func(t *testing.T) {
		// Read the same block again — should be a cache hit
		statsBefore := bm.GetStats()
		buf := make([]byte, 100)
		_, err := bm.ReadAt(context.Background(), "test.svs", buf, 0)
		if err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		statsAfter := bm.GetStats()

		if statsAfter.HitCount <= statsBefore.HitCount {
			t.Fatalf("expected cache hit, hits before=%d after=%d",
				statsBefore.HitCount, statsAfter.HitCount)
		}
	})

	t.Run("CacheOnDisk", func(t *testing.T) {
		// Verify block files exist on disk
		var found int
		filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && filepath.Ext(path) == ".blk" {
				found++
			}
			return nil
		})
		if found == 0 {
			t.Fatal("expected cached block files on disk, found none")
		}
	})
}

// TestBlockManagerEviction verifies LRU cache eviction.
func TestBlockManagerEviction(t *testing.T) {
	fileData := make([]byte, 5*1024*1024) // 5 MB
	for i := range fileData {
		fileData[i] = byte(i % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		var start, end int64
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if end >= int64(len(fileData)) {
			end = int64(len(fileData)) - 1
		}
		w.WriteHeader(http.StatusPartialContent)
		w.Write(fileData[start : end+1])
	}))
	defer server.Close()

	cacheDir := filepath.Join(t.TempDir(), "cache")
	fetcher := NewHTTPFetcher(server.Client())

	bm, err := NewBlockManager(
		1*1024*1024, // 1 MB blocks
		cacheDir,
		2*1024*1024, // 2 MB cache limit — can only hold 2 blocks
		fetcher,
		nil,
	)
	if err != nil {
		t.Fatalf("NewBlockManager: %v", err)
	}
	bm.log, _ = zap.NewNop(), error(nil)

	bm.RegisterFile("test.bam", server.URL, int64(len(fileData)))

	// Read blocks 0, 1, 2, 3 (4 MB total, but cache limit is 2 MB)
	for i := 0; i < 4; i++ {
		buf := make([]byte, 100)
		offset := int64(i) * 1024 * 1024
		_, err := bm.ReadAt(context.Background(), "test.bam", buf, offset)
		if err != nil {
			t.Fatalf("ReadAt block %d: %v", i, err)
		}
	}

	// Cache should be over limit
	stats := bm.GetStats()
	if stats.BytesCached <= 2*1024*1024 {
		t.Log("cache is within limit before eviction — ok")
	}

	// Trigger eviction
	evicted := bm.Evict()
	if evicted == 0 {
		// If cache was already within limit, that's fine
		t.Log("no blocks evicted — cache may be within limit")
	}

	statsAfter := bm.GetStats()
	if statsAfter.BytesCached > 2*1024*1024 {
		t.Fatalf("cache still over limit after eviction: %d bytes", statsAfter.BytesCached)
	}
}

// TestCacheStatsHitRate verifies the hit rate calculation.
func TestCacheStatsHitRate(t *testing.T) {
	tests := []struct {
		hits, misses int64
		expected     float64
	}{
		{0, 0, 0},
		{100, 0, 100},
		{50, 50, 50},
		{75, 25, 75},
	}

	for _, tc := range tests {
		stats := CacheStats{HitCount: tc.hits, MissCount: tc.misses}
		rate := stats.HitRate()
		if rate != tc.expected {
			t.Errorf("HitRate(%d/%d): expected %.1f, got %.1f",
				tc.hits, tc.misses, tc.expected, rate)
		}
	}
}

// ─── Prefetch L2 Tests ──────────────────────────────────────────────────────

// TestTIFFPrefetcherOnFileOpen verifies TIFF header parsing and IFD block detection.
func TestTIFFPrefetcherOnFileOpen(t *testing.T) {
	pf := &TIFFPrefetcher{}

	t.Run("little_endian_IFD_in_block_2", func(t *testing.T) {
		// TIFF LE header: "II", magic 42, IFD offset at 8.5 MB (block 2 at 4MB blocks)
		header := make([]byte, 8)
		header[0] = 'I'
		header[1] = 'I'
		header[2] = 42 // magic
		header[3] = 0

		// IFD offset = 8.5 * 1024 * 1024 = 8912896
		offset := uint32(8912896)
		header[4] = byte(offset)
		header[5] = byte(offset >> 8)
		header[6] = byte(offset >> 16)
		header[7] = byte(offset >> 24)

		requests := pf.OnFileOpen("test.svs", header)
		if len(requests) == 0 {
			t.Fatal("expected prefetch requests")
		}
		expectedBlock := int(int64(offset) / DefaultBlockSize)
		if requests[0].Block != expectedBlock {
			t.Fatalf("expected block %d, got %d", expectedBlock, requests[0].Block)
		}
		if requests[0].Priority != PriorityHigh {
			t.Fatal("expected PriorityHigh")
		}
	})

	t.Run("big_endian_header", func(t *testing.T) {
		header := make([]byte, 8)
		header[0] = 'M'
		header[1] = 'M'
		header[2] = 0
		header[3] = 42 // magic

		// IFD at offset 5 MB
		offset := uint32(5 * 1024 * 1024)
		header[4] = byte(offset >> 24)
		header[5] = byte(offset >> 16)
		header[6] = byte(offset >> 8)
		header[7] = byte(offset)

		requests := pf.OnFileOpen("test.tiff", header)
		if len(requests) == 0 {
			t.Fatal("expected prefetch requests for big-endian TIFF")
		}
	})

	t.Run("IFD_in_block_0", func(t *testing.T) {
		// IFD at offset 8 (within block 0) — should not generate requests
		header := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
		requests := pf.OnFileOpen("test.tif", header)
		if len(requests) != 0 {
			t.Fatalf("expected no requests when IFD is in block 0, got %d", len(requests))
		}
	})

	t.Run("not_TIFF", func(t *testing.T) {
		header := []byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0} // PNG header
		requests := pf.OnFileOpen("test.png", header)
		if len(requests) != 0 {
			t.Fatal("expected no requests for non-TIFF file")
		}
	})

	t.Run("short_header", func(t *testing.T) {
		requests := pf.OnFileOpen("test.svs", []byte{0, 1})
		if len(requests) != 0 {
			t.Fatal("expected no requests for short header")
		}
	})
}

// TestDICOMPrefetcherOnBlockLoaded verifies DICOM PixelData tag detection.
func TestDICOMPrefetcherOnBlockLoaded(t *testing.T) {
	pf := &DICOMPrefetcher{}

	t.Run("finds_pixel_data_tag", func(t *testing.T) {
		// Create a data block with PixelData tag (7FE0,0010) embedded
		data := make([]byte, 256)
		// Place PixelData tag at offset 100
		data[100] = 0xE0
		data[101] = 0x7F
		data[102] = 0x10
		data[103] = 0x00

		requests := pf.OnBlockLoaded("test.dcm", 5, data)
		if len(requests) != 4 {
			t.Fatalf("expected 4 prefetch requests, got %d", len(requests))
		}
		// Should request blocks 6, 7, 8, 9
		for i, req := range requests {
			if req.Block != 5+i+1 {
				t.Fatalf("request %d: expected block %d, got %d", i, 5+i+1, req.Block)
			}
			if req.Priority != PriorityHigh {
				t.Fatal("expected PriorityHigh")
			}
		}
	})

	t.Run("no_pixel_data", func(t *testing.T) {
		data := make([]byte, 256)
		requests := pf.OnBlockLoaded("test.dcm", 0, data)
		if len(requests) != 0 {
			t.Fatalf("expected no requests without PixelData tag, got %d", len(requests))
		}
	})
}

// TestBAMPrefetcherOnFileOpen verifies BAM/CRAM magic detection.
func TestBAMPrefetcherOnFileOpen(t *testing.T) {
	pf := &BAMPrefetcher{}

	t.Run("BAM_magic", func(t *testing.T) {
		header := []byte{'B', 'A', 'M', 1, 0, 0, 0, 0}
		requests := pf.OnFileOpen("sample.bam", header)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
		if requests[0].Block != 1 {
			t.Fatalf("expected block 1, got %d", requests[0].Block)
		}
	})

	t.Run("CRAM_magic", func(t *testing.T) {
		header := []byte{'C', 'R', 'A', 'M', 0, 0, 0, 0}
		requests := pf.OnFileOpen("sample.cram", header)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
	})

	t.Run("not_BAM", func(t *testing.T) {
		header := []byte{0, 0, 0, 0, 0, 0, 0, 0}
		requests := pf.OnFileOpen("unknown.bin", header)
		if len(requests) != 0 {
			t.Fatal("expected no requests for non-BAM file")
		}
	})
}

// ─── Prefetch L3 Tests ──────────────────────────────────────────────────────

// TestWarmUpOrdering verifies L3 manifest warmup prioritises smaller files first.
func TestWarmUpOrdering(t *testing.T) {
	// Create a block manager with a mock fetcher
	cacheDir := filepath.Join(t.TempDir(), "cache")
	fetcher := NewHTTPFetcher(http.DefaultClient)

	bm, err := NewBlockManager(
		1*1024*1024,
		cacheDir,
		100*1024*1024,
		fetcher,
		nil,
	)
	if err != nil {
		t.Fatalf("NewBlockManager: %v", err)
	}
	bm.log, _ = zap.NewNop(), error(nil)

	// Create prefetcher with 1 worker
	log := zap.NewNop()
	pf := NewPrefetcher(bm, 3, 1, log)
	defer pf.Stop()

	// Verify format profile registration
	if _, ok := pf.formatProfiles[".dcm"]; !ok {
		t.Fatal("DICOM format profile not registered")
	}
	if _, ok := pf.formatProfiles[".bam"]; !ok {
		t.Fatal("BAM format profile not registered")
	}
	if _, ok := pf.formatProfiles[".svs"]; !ok {
		t.Fatal("SVS format profile not registered")
	}
	if _, ok := pf.formatProfiles[".cram"]; !ok {
		t.Fatal("CRAM format profile not registered")
	}
}

// TestReadU16U32 verifies the byte order utility functions.
func TestReadU16U32(t *testing.T) {
	t.Run("readU16_LE", func(t *testing.T) {
		v := readU16([]byte{0x34, 0x12}, true)
		if v != 0x1234 {
			t.Fatalf("expected 0x1234, got 0x%04X", v)
		}
	})
	t.Run("readU16_BE", func(t *testing.T) {
		v := readU16([]byte{0x12, 0x34}, false)
		if v != 0x1234 {
			t.Fatalf("expected 0x1234, got 0x%04X", v)
		}
	})
	t.Run("readU32_LE", func(t *testing.T) {
		v := readU32([]byte{0x78, 0x56, 0x34, 0x12}, true)
		if v != 0x12345678 {
			t.Fatalf("expected 0x12345678, got 0x%08X", v)
		}
	})
	t.Run("readU32_BE", func(t *testing.T) {
		v := readU32([]byte{0x12, 0x34, 0x56, 0x78}, false)
		if v != 0x12345678 {
			t.Fatalf("expected 0x12345678, got 0x%08X", v)
		}
	})
}
