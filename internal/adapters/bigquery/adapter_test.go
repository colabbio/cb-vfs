package bigquery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap/zaptest"
	"google.golang.org/api/iterator"
)

func TestValueToString(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{"nil", nil, ""},
		{"string", "TCGA-PRAD", "TCGA-PRAD"},
		{"int", 42, "42"},
		{"int64", int64(987654321), "987654321"},
		{"float64", 3.14159, "3.14159"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"time.Time", time.Date(2026, 10, 17, 14, 30, 0, 0, time.UTC), "2026-10-17T14:30:00Z"},
		{"civil.Date", civil.Date{Year: 2026, Month: 10, Day: 17}, "2026-10-17"},
		{"civil.DateTime", civil.DateTime{Date: civil.Date{Year: 2026, Month: 10, Day: 17}, Time: civil.Time{Hour: 15, Minute: 0, Second: 0}}, "2026-10-17T15:00:00"},
		{"null string valid", bigquery.NullString{StringVal: "valid", Valid: true}, "valid"},
		{"null string invalid", bigquery.NullString{Valid: false}, ""},
		{"null int64 valid", bigquery.NullInt64{Int64: 100, Valid: true}, "100"},
		{"null int64 invalid", bigquery.NullInt64{Valid: false}, ""},
		{"null float64 valid", bigquery.NullFloat64{Float64: 9.5, Valid: true}, "9.5"},
		{"null bool valid", bigquery.NullBool{Bool: true, Valid: true}, "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValueToString(tt.input)
			if got != tt.want {
				t.Errorf("ValueToString(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// mockIterator implements RowIterator for testing.
type mockIterator struct {
	schema bigquery.Schema
	rows   [][]bigquery.Value
	idx    int
}

func (m *mockIterator) Next(dst any) error {
	if m.idx >= len(m.rows) {
		return iterator.Done
	}
	pRow, ok := dst.(*[]bigquery.Value)
	if !ok {
		return iterator.Done
	}
	*pRow = m.rows[m.idx]
	m.idx++
	return nil
}

func (m *mockIterator) Schema() bigquery.Schema {
	return m.schema
}

// mockClient implements Client for testing.
type mockClient struct {
	it *mockIterator
}

func (m *mockClient) Query(q string, location string) (RowIterator, error) {
	return m.it, nil
}

func (m *mockClient) Close() error {
	return nil
}

func TestBigQueryAdapter_Fetch_WithMock(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := config.SourceConfig{
		ID:   "prad_cohort",
		Type: "bigquery",
		BigQuery: &config.BigQueryConfig{
			ProjectID: "test-gcp-project",
		},
		Query: "SELECT patient_id, gleason_score, bcr_status FROM tcga_prad",
		Materialize: config.MaterializeConfig{
			Output: "clinical/prad_cohort.csv",
		},
	}

	mockIt := &mockIterator{
		schema: bigquery.Schema{
			{Name: "patient_id"},
			{Name: "gleason_score"},
			{Name: "bcr_status"},
		},
		rows: [][]bigquery.Value{
			{"TCGA-HC-7740", int64(7), "NO"},
			{"TCGA-HC-7742", int64(9), "YES"},
			{"TCGA-HC-7744", int64(6), "NO"},
		},
	}

	adapter := NewWithClient(cfg, zaptest.NewLogger(t), &mockClient{it: mockIt})
	progress := make(chan adapters.Event, 10)

	err := adapter.Fetch(context.Background(), tmpDir, progress)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	csvFile := filepath.Join(tmpDir, "clinical", "prad_cohort.csv")
	content, err := os.ReadFile(csvFile)
	if err != nil {
		t.Fatalf("could not read output file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 4 { // 1 header + 3 rows
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), string(content))
	}

	if lines[0] != "patient_id,gleason_score,bcr_status" {
		t.Errorf("unexpected header: %q", lines[0])
	}
	if lines[1] != "TCGA-HC-7740,7,NO" {
		t.Errorf("unexpected row 1: %q", lines[1])
	}
	if lines[2] != "TCGA-HC-7742,9,YES" {
		t.Errorf("unexpected row 2: %q", lines[2])
	}
}

func TestResolveProjectID(t *testing.T) {
	// 1. From config
	cfg1 := config.SourceConfig{
		ID:       "s1",
		BigQuery: &config.BigQueryConfig{ProjectID: "proj-123"},
	}
	a1 := New(cfg1, zaptest.NewLogger(t))
	p1, err := a1.resolveProjectID()
	if err != nil || p1 != "proj-123" {
		t.Errorf("got %q, err %v; want proj-123", p1, err)
	}

	// 2. From env
	_ = os.Setenv("GOOGLE_CLOUD_PROJECT", "env-proj-456")
	defer os.Unsetenv("GOOGLE_CLOUD_PROJECT")

	cfg2 := config.SourceConfig{ID: "s2"}
	a2 := New(cfg2, zaptest.NewLogger(t))
	p2, err := a2.resolveProjectID()
	if err != nil || p2 != "env-proj-456" {
		t.Errorf("got %q, err %v; want env-proj-456", p2, err)
	}
}
