package sql

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"
)

// SQLAdapter executes SQL queries against relational databases (PostgreSQL, Cloud SQL)
// and materialises the results into CSV files.
type SQLAdapter struct {
	cfg config.SourceConfig
	dsn string
	log *zap.Logger
}

// New creates a new SQLAdapter.
func New(cfg config.SourceConfig, dsn string, log *zap.Logger) *SQLAdapter {
	return &SQLAdapter{
		cfg: cfg,
		dsn: dsn,
		log: log,
	}
}

func (a *SQLAdapter) ID() string { return a.cfg.ID }

// Validate verifies database connectivity.
func (a *SQLAdapter) Validate(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, a.dsn)
	if err != nil {
		return fmt.Errorf("sql[%s]: connect: %w", a.cfg.ID, err)
	}
	return conn.Close(ctx)
}

// Fetch executes the query and writes the resulting dataset to a CSV file.
func (a *SQLAdapter) Fetch(ctx context.Context, outDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "start", Message: "connecting to SQL database"}

	conn, err := pgx.Connect(ctx, a.dsn)
	if err != nil {
		return fmt.Errorf("sql[%s]: connect: %w", a.cfg.ID, err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, a.cfg.Query)
	if err != nil {
		return fmt.Errorf("sql[%s]: query: %w", a.cfg.ID, err)
	}
	defer rows.Close()

	outPath := filepath.Join(outDir, a.cfg.Materialize.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("sql[%s]: mkdir: %w", a.cfg.ID, err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("sql[%s]: create file %q: %w", a.cfg.ID, outPath, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)

	// Write header from field descriptions
	fields := rows.FieldDescriptions()
	header := make([]string, len(fields))
	for i, fd := range fields {
		header[i] = string(fd.Name)
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("sql[%s]: write header: %w", a.cfg.ID, err)
	}

	count := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return fmt.Errorf("sql[%s]: read row %d: %w", a.cfg.ID, count, err)
		}

		record := make([]string, len(vals))
		for i, v := range vals {
			record[i] = ValueToString(v)
		}

		if err := w.Write(record); err != nil {
			return fmt.Errorf("sql[%s]: write row %d: %w", a.cfg.ID, count, err)
		}

		count++
		if count%1000 == 0 {
			w.Flush()
			progress <- adapters.Event{SourceID: a.cfg.ID, Status: "progress", Rows: count}
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sql[%s]: rows iteration: %w", a.cfg.ID, err)
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("sql[%s]: csv flush: %w", a.cfg.ID, err)
	}

	progress <- adapters.Event{
		SourceID: a.cfg.ID,
		Status:   "done",
		Message:  fmt.Sprintf("wrote %d rows to %s", count, a.cfg.Materialize.Output),
		Rows:     count,
	}
	return nil
}

// ValueToString converts SQL driver values into CSV safe strings.
func ValueToString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%d", t)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", t)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case time.Time:
		return t.Format(time.RFC3339)
	case pgtype.Numeric:
		if !t.Valid {
			return ""
		}
		f, err := t.Float64Value()
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return strconv.FormatFloat(f.Float64, 'f', -1, 64)
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}
