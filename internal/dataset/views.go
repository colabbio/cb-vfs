// Package dataset — views.go implements cross-source CSV joins (derived_views).
// It loads materialised CSVs from multiple adapters, joins them on a common
// key column, and produces a new combined CSV for ML consumption.
package dataset

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

// ─── Derived Views (Cross-Source CSV Join) ────────────────────────────────────

// sourceTable holds the parsed content of a single source CSV.
type sourceTable struct {
	SourceID string
	Header   []string            // original column names
	ByKey    map[string][]string // key_value → row values (same order as Header)
	Keys     []string            // ordered list of unique keys (for deterministic output)
}

// ExecuteDerivedViews processes all derived_views in the dataset config,
// joining materialised CSVs and writing the result to outDir.
func ExecuteDerivedViews(views []config.DerivedViewConfig, sources []config.SourceConfig, outDir string, log *zap.Logger) error {
	// Build a quick lookup: source_id → materialize.output
	sourceOutputs := make(map[string]string)
	for _, src := range sources {
		sourceOutputs[src.ID] = src.Materialize.Output
	}

	for _, view := range views {
		if err := executeSingleView(view, sourceOutputs, outDir, log); err != nil {
			return fmt.Errorf("derived_view %q: %w", view.ID, err)
		}
	}
	return nil
}

func executeSingleView(view config.DerivedViewConfig, sourceOutputs map[string]string, outDir string, log *zap.Logger) error {
	if len(view.Sources) < 2 {
		return fmt.Errorf("need at least 2 sources to join, got %d", len(view.Sources))
	}

	// 1. Load all source CSVs into memory
	tables := make([]*sourceTable, 0, len(view.Sources))
	for _, ref := range view.Sources {
		csvFile, ok := sourceOutputs[ref.Ref]
		if !ok {
			return fmt.Errorf("source %q not found in sources[] config", ref.Ref)
		}

		tbl, err := loadSourceCSV(filepath.Join(outDir, csvFile), ref.Ref, ref.Key)
		if err != nil {
			return fmt.Errorf("load source %q (%s): %w", ref.Ref, csvFile, err)
		}
		tables = append(tables, tbl)

		log.Debug("loaded source for join",
			zap.String("source", ref.Ref),
			zap.Int("rows", len(tbl.Keys)),
			zap.Int("columns", len(tbl.Header)),
		)
	}

	// 2. Compute joined keys based on join type
	joinType := strings.ToLower(view.JoinType)
	if joinType == "" {
		joinType = "left"
	}
	joinedKeys := computeJoinKeys(tables, joinType)

	// 3. Build output header and column mapping
	header, colMap := buildOutputColumns(tables, view.Columns)

	// 4. Write output CSV
	outPath := filepath.Join(outDir, view.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir for output: %w", err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output %q: %w", outPath, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)

	// Write header
	if err := w.Write(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	// Write rows
	rowCount := 0
	for _, key := range joinedKeys {
		row := buildJoinedRow(key, tables, colMap)
		if err := w.Write(row); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
		rowCount++
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("csv flush: %w", err)
	}

	log.Info("derived view written",
		zap.String("view", view.ID),
		zap.String("output", view.Output),
		zap.Int("rows", rowCount),
		zap.Int("columns", len(header)),
		zap.String("join_type", joinType),
	)
	return nil
}

// loadSourceCSV reads a CSV file and indexes rows by the key column.
func loadSourceCSV(path, sourceID, keyColumn string) (*sourceTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	reader := csv.NewReader(f)

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	// Find key column index
	keyIdx := -1
	for i, col := range header {
		if strings.EqualFold(strings.TrimSpace(col), keyColumn) {
			keyIdx = i
			break
		}
	}
	if keyIdx == -1 {
		return nil, fmt.Errorf("key column %q not found in header %v", keyColumn, header)
	}

	tbl := &sourceTable{
		SourceID: sourceID,
		Header:   header,
		ByKey:    make(map[string][]string),
	}

	seenKeys := make(map[string]bool)
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}

		if keyIdx < len(record) {
			key := strings.TrimSpace(record[keyIdx])
			if key != "" {
				tbl.ByKey[key] = record
				if !seenKeys[key] {
					tbl.Keys = append(tbl.Keys, key)
					seenKeys[key] = true
				}
			}
		}
	}

	return tbl, nil
}

// computeJoinKeys returns the ordered list of keys based on join strategy.
func computeJoinKeys(tables []*sourceTable, joinType string) []string {
	switch joinType {
	case "inner":
		// Only keys present in ALL tables
		return intersectKeys(tables)
	case "full":
		// All keys from all tables
		return unionKeys(tables)
	default: // "left"
		// Keys from the first (left) table only
		return tables[0].Keys
	}
}

func intersectKeys(tables []*sourceTable) []string {
	if len(tables) == 0 {
		return nil
	}

	// Start with keys from the first table
	candidates := make(map[string]bool)
	for _, k := range tables[0].Keys {
		candidates[k] = true
	}

	// Intersect with each subsequent table
	for _, tbl := range tables[1:] {
		next := make(map[string]bool)
		for k := range candidates {
			if _, ok := tbl.ByKey[k]; ok {
				next[k] = true
			}
		}
		candidates = next
	}

	// Return in stable order (first table's order)
	var result []string
	for _, k := range tables[0].Keys {
		if candidates[k] {
			result = append(result, k)
		}
	}
	return result
}

func unionKeys(tables []*sourceTable) []string {
	seen := make(map[string]bool)
	var result []string

	for _, tbl := range tables {
		for _, k := range tbl.Keys {
			if !seen[k] {
				result = append(result, k)
				seen[k] = true
			}
		}
	}

	sort.Strings(result)
	return result
}

// ─── Column mapping ──────────────────────────────────────────────────────────

// columnRef identifies a column in a specific source table.
type columnRef struct {
	TableIdx  int    // index into the tables slice
	ColumnIdx int    // index into the table's Header
	Alias     string // output column name
}

// buildOutputColumns parses the column spec and returns the output header
// plus a slice of columnRef mappings.
func buildOutputColumns(tables []*sourceTable, columnSpecs []string) ([]string, []columnRef) {
	if len(columnSpecs) == 0 {
		// No column spec: include all columns from all tables, prefixed
		return buildAllColumns(tables)
	}

	var header []string
	var colMap []columnRef

	for _, spec := range columnSpecs {
		ref, err := parseColumnSpec(spec, tables)
		if err != nil {
			// Skip invalid column specs with a warning (could log)
			continue
		}
		header = append(header, ref.Alias)
		colMap = append(colMap, ref)
	}

	return header, colMap
}

func buildAllColumns(tables []*sourceTable) ([]string, []columnRef) {
	var header []string
	var colMap []columnRef

	for tblIdx, tbl := range tables {
		for colIdx, col := range tbl.Header {
			alias := tbl.SourceID + "." + col
			header = append(header, alias)
			colMap = append(colMap, columnRef{
				TableIdx:  tblIdx,
				ColumnIdx: colIdx,
				Alias:     alias,
			})
		}
	}
	return header, colMap
}

// parseColumnSpec parses "source_id.column_name" or "source_id.column_name AS alias".
func parseColumnSpec(spec string, tables []*sourceTable) (columnRef, error) {
	spec = strings.TrimSpace(spec)

	// Check for AS alias
	alias := ""
	if idx := strings.Index(strings.ToUpper(spec), " AS "); idx >= 0 {
		alias = strings.TrimSpace(spec[idx+4:])
		spec = strings.TrimSpace(spec[:idx])
	}

	// Split source_id.column_name
	parts := strings.SplitN(spec, ".", 2)
	if len(parts) != 2 {
		return columnRef{}, fmt.Errorf("invalid column spec %q: expected source_id.column_name", spec)
	}

	sourceID := parts[0]
	colName := parts[1]

	// Find table
	for tblIdx, tbl := range tables {
		if tbl.SourceID != sourceID {
			continue
		}
		// Find column
		for colIdx, col := range tbl.Header {
			if strings.EqualFold(col, colName) {
				if alias == "" {
					alias = colName
				}
				return columnRef{
					TableIdx:  tblIdx,
					ColumnIdx: colIdx,
					Alias:     alias,
				}, nil
			}
		}
		return columnRef{}, fmt.Errorf("column %q not found in source %q (available: %v)", colName, sourceID, tbl.Header)
	}
	return columnRef{}, fmt.Errorf("source %q not found in joined tables", sourceID)
}

// buildJoinedRow constructs a single output row for the given key.
func buildJoinedRow(key string, tables []*sourceTable, colMap []columnRef) []string {
	row := make([]string, len(colMap))

	for i, ref := range colMap {
		tbl := tables[ref.TableIdx]
		if record, ok := tbl.ByKey[key]; ok && ref.ColumnIdx < len(record) {
			row[i] = record[ref.ColumnIdx]
		} else {
			row[i] = "" // NULL equivalent for missing joins
		}
	}

	return row
}
