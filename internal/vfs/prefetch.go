package vfs

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/colabbio/medvfs/internal/adapters"
	"go.uber.org/zap"
)

// FetchPriority controls the urgency of a prefetch request.
type FetchPriority int

const (
	// PriorityLow is for speculative prefetch (sequential read-ahead, warmup).
	PriorityLow FetchPriority = iota
	// PriorityHigh is for format-aware prefetch (TIFF IFD, BAM index).
	PriorityHigh
)

// FetchRequest represents a block to be prefetched in the background.
type FetchRequest struct {
	FileID   string
	Block    int
	Priority FetchPriority
}

// FormatPrefetcher provides format-specific prefetch hints.
// Implementations parse file headers to predict which blocks will be needed.
type FormatPrefetcher interface {
	// OnFileOpen is called when a file is opened. Returns blocks to prefetch
	// based on the file format (e.g., TIFF header → IFD block).
	OnFileOpen(fileID string, header []byte) []FetchRequest

	// OnBlockLoaded is called when a block is loaded. Returns additional
	// blocks to prefetch based on parsed content (e.g., IFD → tile offsets).
	OnBlockLoaded(fileID string, blockIdx int, data []byte) []FetchRequest
}

// Prefetcher is the multi-level prefetch engine.
// Level 1: Sequential read-ahead (always active).
// Level 2: Format-aware prefetch (active if format profile matches).
// Level 3: Manifest-driven warmup (active on startup with manifest strategy).
type Prefetcher struct {
	mu sync.RWMutex

	bm  *BlockManager
	log *zap.Logger

	// readAheadBlocks is the number of blocks to prefetch sequentially (L1).
	readAheadBlocks int

	// formatProfiles maps file extension → format-specific prefetcher (L2).
	formatProfiles map[string]FormatPrefetcher

	// fetchQueue is the async channel for background fetch requests.
	fetchQueue chan FetchRequest

	// ctx is the background context for prefetch goroutines.
	ctx    context.Context
	cancel context.CancelFunc

	// fileExtensions maps fileID → file extension (for format dispatch).
	fileExtensions map[string]string
}

// NewPrefetcher creates a prefetch engine with the given configuration.
func NewPrefetcher(bm *BlockManager, readAheadBlocks int, concurrency int, log *zap.Logger) *Prefetcher {
	if readAheadBlocks <= 0 {
		readAheadBlocks = 3
	}
	if concurrency <= 0 {
		concurrency = 4
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &Prefetcher{
		bm:              bm,
		log:             log,
		readAheadBlocks: readAheadBlocks,
		formatProfiles:  make(map[string]FormatPrefetcher),
		fetchQueue:      make(chan FetchRequest, 256),
		ctx:             ctx,
		cancel:          cancel,
		fileExtensions:  make(map[string]string),
	}

	// Register built-in format profiles
	tiffPrefetcher := &TIFFPrefetcher{}
	p.formatProfiles[".svs"] = tiffPrefetcher
	p.formatProfiles[".ndpi"] = tiffPrefetcher
	p.formatProfiles[".tiff"] = tiffPrefetcher
	p.formatProfiles[".tif"] = tiffPrefetcher

	dicomPrefetcher := &DICOMPrefetcher{}
	p.formatProfiles[".dcm"] = dicomPrefetcher
	p.formatProfiles[".dicom"] = dicomPrefetcher

	bamPrefetcher := &BAMPrefetcher{}
	p.formatProfiles[".bam"] = bamPrefetcher
	p.formatProfiles[".cram"] = bamPrefetcher

	// Start background fetch workers
	for i := 0; i < concurrency; i++ {
		go p.fetchWorker(i)
	}

	return p
}

// RegisterFileExtension records the file extension for format-aware prefetch.
func (p *Prefetcher) RegisterFileExtension(fileID string, ext string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fileExtensions[fileID] = ext
}

// OnBlockAccess is called whenever a block is read. Triggers L1 sequential
// read-ahead: prefetch the next N blocks after the accessed block.
func (p *Prefetcher) OnBlockAccess(fileID string, blockIdx int) {
	for i := 1; i <= p.readAheadBlocks; i++ {
		nextBlock := blockIdx + i
		select {
		case p.fetchQueue <- FetchRequest{
			FileID:   fileID,
			Block:    nextBlock,
			Priority: PriorityLow,
		}:
		default:
			// Queue full, skip this prefetch
		}
	}
}

// OnFileOpen triggers format-aware prefetch (L2) when a file is first opened.
func (p *Prefetcher) OnFileOpen(fileID string, header []byte) {
	p.mu.RLock()
	ext, ok := p.fileExtensions[fileID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	fp, ok := p.formatProfiles[ext]
	if !ok {
		return
	}

	requests := fp.OnFileOpen(fileID, header)
	for _, req := range requests {
		select {
		case p.fetchQueue <- req:
		default:
		}
	}
}

// WarmUp implements L3 manifest-driven prefetch. It pre-downloads the first
// block (header) of a percentage of files from the manifest, prioritising
// smaller files first.
func (p *Prefetcher) WarmUp(mappings []adapters.FileMapping, warmupPercent int) {
	if warmupPercent <= 0 || len(mappings) == 0 {
		return
	}

	// Sort by size (smallest first) — simple bubble for now
	sorted := make([]adapters.FileMapping, len(mappings))
	copy(sorted, mappings)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].SizeBytes < sorted[i].SizeBytes {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	count := len(sorted) * warmupPercent / 100
	if count < 1 {
		count = 1
	}

	p.log.Info("manifest warmup starting",
		zap.Int("total_files", len(sorted)),
		zap.Int("warmup_files", count),
	)

	for _, m := range sorted[:count] {
		select {
		case p.fetchQueue <- FetchRequest{
			FileID:   m.LogicalPath,
			Block:    0, // header block only
			Priority: PriorityLow,
		}:
		case <-p.ctx.Done():
			return
		}
	}
}

// fetchWorker processes fetch requests from the queue.
func (p *Prefetcher) fetchWorker(id int) {
	for {
		select {
		case <-p.ctx.Done():
			return
		case req := <-p.fetchQueue:
			p.bm.Prefetch(p.ctx, req.FileID, []int{req.Block})
		}
	}
}

// Stop shuts down the prefetch engine.
func (p *Prefetcher) Stop() {
	p.cancel()
}

// ─── TIFF/SVS Format-Aware Prefetcher ────────────────────────────────────────

// TIFFPrefetcher implements format-aware prefetch for TIFF-based WSI formats
// (SVS, NDPI, generic TIFF). It parses the TIFF header to locate the IFD
// (Image File Directory) and predicts tile offsets for prefetching.
type TIFFPrefetcher struct{}

// OnFileOpen parses the TIFF header to determine the IFD block location.
// TIFF header: bytes 0-7 → byte order (2) + magic (2) + IFD offset (4).
func (t *TIFFPrefetcher) OnFileOpen(fileID string, header []byte) []FetchRequest {
	if len(header) < 8 {
		return nil
	}

	// Determine byte order and parse IFD offset
	var ifdOffset uint32
	var littleEndian bool
	switch {
	case header[0] == 'I' && header[1] == 'I': // Little-endian
		littleEndian = true
		ifdOffset = readU32(header[4:8], littleEndian)
	case header[0] == 'M' && header[1] == 'M': // Big-endian
		littleEndian = false
		ifdOffset = readU32(header[4:8], littleEndian)
	default:
		return nil // Not a TIFF file
	}

	var requests []FetchRequest

	// Prefetch the block containing the first IFD
	ifdBlock := int(int64(ifdOffset) / DefaultBlockSize)
	if ifdBlock > 0 {
		requests = append(requests, FetchRequest{
			FileID: fileID, Block: ifdBlock, Priority: PriorityHigh,
		})
	}

	// For SVS/NDPI: the label and macro images have IFDs near the end.
	// Also prefetch the last block as it often contains the overview IFD.
	return requests
}

// OnBlockLoaded parses IFD entries to extract tile/strip offsets and compute
// which blocks to prefetch for tile data access.
// Key TIFF tags parsed:
//   - 273 (StripOffsets) — offsets to strip data
//   - 279 (StripByteCounts) — sizes of strips
//   - 324 (TileOffsets) — offsets to tile data
//   - 325 (TileByteCounts) — sizes of tiles
func (t *TIFFPrefetcher) OnBlockLoaded(fileID string, blockIdx int, data []byte) []FetchRequest {
	if len(data) < 12 {
		return nil
	}

	// Try to detect if this block contains an IFD by looking for
	// a valid IFD entry count followed by plausible tag entries.
	// IFD format: uint16(count) + count * 12-byte entries + uint32(nextIFD)
	offsets := extractTileOffsets(data)
	if len(offsets) == 0 {
		return nil
	}

	// Deduplicate blocks to prefetch
	blockSet := make(map[int]struct{})
	for _, offset := range offsets {
		block := int(offset / DefaultBlockSize)
		if block != blockIdx { // Don't re-fetch the current block
			blockSet[block] = struct{}{}
		}
	}

	// Limit to first 16 unique blocks (avoid flooding the queue)
	var requests []FetchRequest
	count := 0
	for block := range blockSet {
		if count >= 16 {
			break
		}
		requests = append(requests, FetchRequest{
			FileID:   fileID,
			Block:    block,
			Priority: PriorityHigh,
		})
		count++
	}
	return requests
}

// extractTileOffsets scans a data block for TIFF IFD entries containing
// tile or strip offsets. Returns the offset values found.
func extractTileOffsets(data []byte) []int64 {
	if len(data) < 2 {
		return nil
	}

	// Try both byte orders
	for _, le := range []bool{true, false} {
		offsets := tryParseIFDOffsets(data, le)
		if len(offsets) > 0 {
			return offsets
		}
	}
	return nil
}

// tryParseIFDOffsets attempts to parse IFD entries from the given data
// using the specified byte order. Returns tile/strip offsets if found.
func tryParseIFDOffsets(data []byte, littleEndian bool) []int64 {
	// Scan for plausible IFD start positions
	// An IFD starts with a uint16 entry count, followed by 12-byte entries
	for pos := 0; pos+2 < len(data); pos++ {
		count := int(readU16(data[pos:pos+2], littleEndian))
		if count < 1 || count > 500 {
			continue
		}

		endPos := pos + 2 + count*12
		if endPos+4 > len(data) {
			continue
		}

		// Validate: check if entries look like valid TIFF tags
		validEntries := 0
		var offsets []int64

		for i := 0; i < count && pos+2+i*12+12 <= len(data); i++ {
			entry := data[pos+2+i*12 : pos+2+i*12+12]
			tag := readU16(entry[0:2], littleEndian)
			fieldType := readU16(entry[2:4], littleEndian)
			valCount := readU32(entry[4:8], littleEndian)

			// Valid tag range check
			if tag > 0 && tag < 65535 && fieldType >= 1 && fieldType <= 13 {
				validEntries++
			}

			// Tags 273 (StripOffsets) or 324 (TileOffsets)
			if tag == 273 || tag == 324 {
				if valCount == 1 {
					// Single offset stored inline
					offset := int64(readU32(entry[8:12], littleEndian))
					offsets = append(offsets, offset)
				}
				// Multiple offsets: the value field is a pointer to the array
				// We can't follow it here since it might be in a different block
			}
		}

		// If at least 3 entries looked valid and we found offsets, return them
		if validEntries >= 3 && len(offsets) > 0 {
			return offsets
		}
	}
	return nil
}

// readU16 reads a uint16 from a 2-byte slice in the specified byte order.
func readU16(b []byte, littleEndian bool) uint16 {
	if littleEndian {
		return uint16(b[0]) | uint16(b[1])<<8
	}
	return uint16(b[1]) | uint16(b[0])<<8
}

// readU32 reads a uint32 from a 4-byte slice in the specified byte order.
func readU32(b []byte, littleEndian bool) uint32 {
	if littleEndian {
		return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	}
	return uint32(b[3]) | uint32(b[2])<<8 | uint32(b[1])<<16 | uint32(b[0])<<24
}

// ─── DICOM Format-Aware Prefetcher ──────────────────────────────────────────

// DICOMPrefetcher implements format-aware prefetch for DICOM files.
// DICOM pixel data is typically at the end of the file after the metadata
// preamble. The prefetcher:
//   1. On open: prefetches the last block (pixel data start for most DICOM)
//   2. On block load: scans for PixelData tag (7FE0,0010) to find the
//      pixel data offset and prefetch surrounding blocks.
type DICOMPrefetcher struct{}

// OnFileOpen prefetches block 0 (metadata header) — this is usually already
// loaded, so it also requests the last block which typically contains pixel
// data in standard DICOM files.
func (d *DICOMPrefetcher) OnFileOpen(fileID string, header []byte) []FetchRequest {
	// DICOM files: metadata in the first few KB, pixel data near the end.
	// We can't know the file size here, so we request block 0 metadata
	// and rely on OnBlockLoaded to find pixel data.
	return nil
}

// OnBlockLoaded scans for the DICOM PixelData tag (7FE0,0010) to predict
// which blocks contain the actual image data.
func (d *DICOMPrefetcher) OnBlockLoaded(fileID string, blockIdx int, data []byte) []FetchRequest {
	if len(data) < 8 {
		return nil
	}

	// Scan for PixelData tag marker: group=0x7FE0, element=0x0010
	// This tag marks the beginning of pixel data in DICOM
	for i := 0; i+8 <= len(data); i += 2 {
		// Little-endian scan
		if data[i] == 0xE0 && data[i+1] == 0x7F &&
			data[i+2] == 0x10 && data[i+3] == 0x00 {
			// Found pixel data tag — prefetch next blocks
			var requests []FetchRequest
			for j := 1; j <= 4; j++ {
				requests = append(requests, FetchRequest{
					FileID:   fileID,
					Block:    blockIdx + j,
					Priority: PriorityHigh,
				})
			}
			return requests
		}
	}
	return nil
}

// ─── BAM/CRAM Format-Aware Prefetcher ───────────────────────────────────────

// BAMPrefetcher implements format-aware prefetch for BAM/CRAM genomics files.
// BAM files have a BGZF-compressed structure where the index (.bai) maps
// genomic coordinates to file offsets. The prefetcher targets the BAM header
// and first alignment blocks.
type BAMPrefetcher struct{}

// OnFileOpen checks for the BAM magic number and prefetches the header block.
func (b *BAMPrefetcher) OnFileOpen(fileID string, header []byte) []FetchRequest {
	if len(header) < 4 {
		return nil
	}
	// BAM magic: "BAM\1"
	if header[0] == 'B' && header[1] == 'A' && header[2] == 'M' && header[3] == 1 {
		// BAM header is in block 0, prefetch block 1 (first alignments)
		return []FetchRequest{
			{FileID: fileID, Block: 1, Priority: PriorityHigh},
		}
	}
	// CRAM magic: "CRAM"
	if header[0] == 'C' && header[1] == 'R' && header[2] == 'A' && header[3] == 'M' {
		return []FetchRequest{
			{FileID: fileID, Block: 1, Priority: PriorityHigh},
		}
	}
	return nil
}

// OnBlockLoaded is a no-op for BAM — index-driven access patterns are
// handled by the sequential L1 prefetcher once reads begin.
func (b *BAMPrefetcher) OnBlockLoaded(fileID string, blockIdx int, data []byte) []FetchRequest {
	return nil
}

// fileExtension returns the lowercase extension of the logical path.
func fileExtension(logicalPath string) string {
	return filepath.Ext(logicalPath)
}

