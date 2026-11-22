package vfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/colabbio/medvfs/internal/adapters"
)

// NfCoreSamplesheetRequest describes a cohort-scoped nf-core samplesheet generation request.
type NfCoreSamplesheetRequest struct {
	GrantID            string            `json:"grantId"`
	CohortID           string            `json:"cohortId"`
	Pipeline           string            `json:"pipeline"`
	MountPrefix        string            `json:"mountPrefix"`
	OutputPath         string            `json:"outputPath,omitempty"`
	SamplesheetMapping map[string]string `json:"samplesheetMapping,omitempty"`
}

// PseudonymizePatientID returns a deterministic PSEUD-XXXX identifier so raw hospital MRNs never leak into nf-core.
func PseudonymizePatientID(rawID string) string {
	trimmed := strings.TrimSpace(rawID)
	if strings.HasPrefix(trimmed, "PSEUD-") {
		return trimmed
	}
	sum := sha256.Sum256([]byte("colabbio-nfcore-salt:" + trimmed))
	return "PSEUD-" + strings.ToUpper(hex.EncodeToString(sum[:])[:8])
}

// SynthesizeNfCoreSamplesheet builds an nf-schema compliant, DPO-grant filtered samplesheet.csv
// from the registered MedVFS file mappings (CB-SPEC-NFCORE-024 Bridge 2 / colabbio/cb-vfs#7).
func SynthesizeNfCoreSamplesheet(
	acl *CohortAclRegistry,
	mappings []adapters.FileMapping,
	req NfCoreSamplesheetRequest,
) (string, int, error) {
	if acl == nil {
		return "", 0, fmt.Errorf("cohort ACL registry is not initialized")
	}
	if req.GrantID == "" {
		return "", 0, fmt.Errorf("missing X-ColabBio-Grant-Id / grantId")
	}
	rule, ok := acl.GetGrant(req.GrantID)
	if !ok || rule.Revoked {
		return "", 0, fmt.Errorf("%w: grant %q is not active", ErrAccessRevoked, req.GrantID)
	}

	mountPrefix := req.MountPrefix
	if mountPrefix == "" {
		mountPrefix = "/mnt/medvfs"
	}
	mountPrefix = strings.TrimSuffix(filepath.ToSlash(mountPrefix), "/")

	type pair struct {
		patient string
		sample  string
		r1      string
		r2      string
		single  string
	}
	bySample := make(map[string]*pair)

	for _, m := range mappings {
		if !acl.IsAllowed(req.GrantID, m.LogicalPath) {
			continue
		}
		clean := filepath.ToSlash(m.LogicalPath)
		rawPatient := extractPatientToken(clean, rule.AllowedPatients)
		pseudPatient := PseudonymizePatientID(rawPatient)
		base := filepath.Base(clean)
		sampleKey := deriveSampleKey(pseudPatient, base)

		entry, exists := bySample[sampleKey]
		if !exists {
			entry = &pair{
				patient: pseudPatient,
				sample:  sampleKey,
			}
			bySample[sampleKey] = entry
		}
		fullVirtualPath := mountPrefix + "/" + strings.TrimPrefix(clean, "/")
		switch {
		case strings.Contains(base, "_R1") || strings.Contains(base, "_1.fastq"):
			entry.r1 = fullVirtualPath
		case strings.Contains(base, "_R2") || strings.Contains(base, "_2.fastq"):
			entry.r2 = fullVirtualPath
		default:
			if entry.r1 == "" {
				entry.r1 = fullVirtualPath
			}
			entry.single = fullVirtualPath
		}
	}

	keys := make([]string, 0, len(bySample))
	for k := range bySample {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	isSarek := strings.Contains(strings.ToLower(req.Pipeline), "sarek")
	if isSarek {
		_ = writer.Write([]string{"patient", "sample", "lane", "fastq_1", "fastq_2"})
	} else {
		_ = writer.Write([]string{"sample", "fastq_1", "fastq_2", "strandedness"})
	}

	rows := 0
	for _, k := range keys {
		e := bySample[k]
		if e.r1 == "" {
			continue
		}
		if isSarek {
			_ = writer.Write([]string{e.patient, e.sample, "L001", e.r1, e.r2})
		} else {
			_ = writer.Write([]string{e.sample, e.r1, e.r2, "auto"})
		}
		rows++
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", 0, err
	}

	csvContent := buf.String()
	if req.OutputPath != "" {
		if err := os.MkdirAll(filepath.Dir(req.OutputPath), 0o755); err != nil {
			return "", 0, err
		}
		if err := os.WriteFile(req.OutputPath, []byte(csvContent), 0o600); err != nil {
			return "", 0, err
		}
	}
	return csvContent, rows, nil
}

func extractPatientToken(logicalPath string, allowedPatients []string) string {
	for _, p := range allowedPatients {
		if p != "" && strings.Contains(logicalPath, p) {
			return p
		}
	}
	parts := strings.Split(strings.Trim(logicalPath, "/"), "/")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return "SUBJECT"
}

func deriveSampleKey(pseudPatient, base string) string {
	stem := base
	for _, suf := range []string{"_R1.fastq.gz", "_R2.fastq.gz", "_1.fastq.gz", "_2.fastq.gz", ".fastq.gz", ".bam", ".cram", ".svs", ".h5ad"} {
		if strings.HasSuffix(stem, suf) {
			stem = strings.TrimSuffix(stem, suf)
			break
		}
	}
	if stem == "" {
		return pseudPatient + "-S1"
	}
	return pseudPatient + "_" + stem
}
