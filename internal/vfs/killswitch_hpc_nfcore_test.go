package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"go.uber.org/zap"
)

type staticMemFetcher struct {
	payload []byte
}

func (f *staticMemFetcher) FetchBlock(_ context.Context, _ string, offset, length int64) ([]byte, error) {
	if offset >= int64(len(f.payload)) {
		return nil, nil
	}
	end := offset + length
	if end > int64(len(f.payload)) {
		end = int64(len(f.payload))
	}
	return append([]byte(nil), f.payload[offset:end]...), nil
}

func (f *staticMemFetcher) FileSize(_ context.Context, _ string) (int64, error) {
	return int64(len(f.payload)), nil
}

func TestHotKillSwitchEvictsBlockCacheAndBlocksReads(t *testing.T) {
	tmpDir := t.TempDir()
	log := zap.NewNop()
	fetcher := &staticMemFetcher{payload: []byte("SECRET_GERMLINE_BAM_CONTENT_12345")}
	bm, err := NewBlockManager(1024, tmpDir, 10*1024*1024, fetcher, log)
	if err != nil {
		t.Fatalf("NewBlockManager: %v", err)
	}

	fileAllowed := "cohorts/prad/PT-001/tumor.bam"
	fileOther := "cohorts/brca/PT-999/normal.bam"
	bm.RegisterFile(fileAllowed, "file:///tmp/tumor.bam", int64(len(fetcher.payload)))
	bm.RegisterFile(fileOther, "file:///tmp/normal.bam", int64(len(fetcher.payload)))

	buf := make([]byte, len(fetcher.payload))
	n, err := bm.ReadAt(context.Background(), fileAllowed, buf, 0)
	if err != nil || n != len(fetcher.payload) {
		t.Fatalf("initial ReadAt failed: n=%d err=%v", n, err)
	}

	acl := NewCohortAclRegistry()
	acl.SyncGrant(CohortGrantRule{
		GrantID:         "GRANT-LAFE-01",
		AllowedPaths:    []string{"cohorts/prad"},
		AllowedPatients: []string{"PT-001"},
		ExpiresAt:       time.Now().Add(time.Hour),
	})

	var evictedFiles, evictedBlocks int
	acl.RegisterRevokeListener(func(rule CohortGrantRule) {
		evictedFiles, evictedBlocks = bm.RevokeAndEvictByGrant(rule)
	})

	acl.RevokeGrant("GRANT-LAFE-01", "DPO immediate revocation")
	if evictedFiles != 1 || evictedBlocks != 1 {
		t.Fatalf("expected 1 evicted file and 1 evicted block, got files=%d blocks=%d", evictedFiles, evictedBlocks)
	}

	if !bm.IsFileRevoked(fileAllowed) {
		t.Fatalf("expected %s to be marked revoked", fileAllowed)
	}
	if bm.IsFileRevoked(fileOther) {
		t.Fatalf("expected %s to remain accessible", fileOther)
	}

	_, err = bm.ReadAt(context.Background(), fileAllowed, buf, 0)
	if !errors.Is(err, ErrAccessRevoked) {
		t.Fatalf("expected ErrAccessRevoked after Kill-Switch, got %v", err)
	}
}

func TestUserspaceVFSRootlessStagingAndKillSwitch(t *testing.T) {
	t.Setenv("MEDVFS_USERSPACE_STAGING", "true")
	if strat := SelectStrategy(nil); strat != StrategyDownload {
		t.Fatalf("expected StrategyDownload when MEDVFS_USERSPACE_STAGING=true, got %s", strat)
	}

	stageDir := t.TempDir()
	srcFile := filepath.Join(t.TempDir(), "sample_R1.fastq.gz")
	if err := os.WriteFile(srcFile, []byte("FASTQ_DATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	u, err := NewUserspaceVFS(stageDir, StrategyDownload)
	if err != nil {
		t.Fatalf("NewUserspaceVFS: %v", err)
	}
	err = u.AddSource("seq", []adapters.FileMapping{
		{LogicalPath: "PT-001/sample_R1.fastq.gz", RemoteRef: "file://" + srcFile, SizeBytes: 10},
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	staged := filepath.Join(stageDir, "PT-001", "sample_R1.fastq.gz")
	if _, err := os.Lstat(staged); err != nil {
		t.Fatalf("expected staged file at %s: %v", staged, err)
	}

	evFiles, _ := u.RevokeAndEvictByGrant(CohortGrantRule{
		GrantID:         "GRANT-HPC-01",
		AllowedPatients: []string{"PT-001"},
	})
	if evFiles != 1 {
		t.Fatalf("expected 1 evicted userspace file, got %d", evFiles)
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("expected staged file %s to be removed on Kill-Switch", staged)
	}
}

func TestSynthesizeNfCoreSamplesheetCohortFilteringAndPseudonymization(t *testing.T) {
	acl := NewCohortAclRegistry()
	acl.SyncGrant(CohortGrantRule{
		GrantID:         "GRANT-NFCORE-01",
		AllowedPatients: []string{"MRN-1001"},
		ExpiresAt:       time.Now().Add(time.Hour),
	})

	mappings := []adapters.FileMapping{
		{LogicalPath: "MRN-1001/tumor_R1.fastq.gz", RemoteRef: "file:///nas/1_R1.fastq.gz"},
		{LogicalPath: "MRN-1001/tumor_R2.fastq.gz", RemoteRef: "file:///nas/1_R2.fastq.gz"},
		{LogicalPath: "MRN-9999-OUTSIDE-COHORT/tumor_R1.fastq.gz", RemoteRef: "file:///nas/9_R1.fastq.gz"},
	}

	outCSV := filepath.Join(t.TempDir(), "samplesheet.csv")
	csvText, rows, err := SynthesizeNfCoreSamplesheet(acl, mappings, NfCoreSamplesheetRequest{
		GrantID:     "GRANT-NFCORE-01",
		CohortID:    "prad-2026",
		Pipeline:    "nf-core/sarek",
		MountPrefix: "/mnt/medvfs",
		OutputPath:  outCSV,
	})
	if err != nil {
		t.Fatalf("SynthesizeNfCoreSamplesheet: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected 1 cohort-approved paired row, got %d:\n%s", rows, csvText)
	}
	if strings.Contains(csvText, "MRN-9999") {
		t.Fatalf("unauthorized patient leaked into samplesheet:\n%s", csvText)
	}
	if !strings.Contains(csvText, "PSEUD-") {
		t.Fatalf("expected pseudonymized patient ID in samplesheet:\n%s", csvText)
	}
	if !strings.Contains(csvText, "/mnt/medvfs/MRN-1001/tumor_R1.fastq.gz,/mnt/medvfs/MRN-1001/tumor_R2.fastq.gz") {
		t.Fatalf("expected paired R1/R2 virtual paths in samplesheet:\n%s", csvText)
	}

	// Revoke grant -> synthesis must fail closed
	acl.RevokeGrant("GRANT-NFCORE-01", "revoked")
	_, _, err = SynthesizeNfCoreSamplesheet(acl, mappings, NfCoreSamplesheetRequest{
		GrantID:  "GRANT-NFCORE-01",
		Pipeline: "nf-core/sarek",
	})
	if !errors.Is(err, ErrAccessRevoked) {
		t.Fatalf("expected ErrAccessRevoked after grant revocation, got %v", err)
	}
}
