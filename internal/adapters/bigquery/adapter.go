package bigquery

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// RowIterator abstracts the BigQuery row iteration for testability.
type RowIterator interface {
	Next(dst any) error
	Schema() bigquery.Schema
}

// Client abstracts the BigQuery client operations.
type Client interface {
	Query(q string, location string) (RowIterator, error)
	Close() error
}

type realRowIterator struct {
	it *bigquery.RowIterator
}

func (r *realRowIterator) Next(dst any) error {
	return r.it.Next(dst)
}

func (r *realRowIterator) Schema() bigquery.Schema {
	return r.it.Schema
}

type realClient struct {
	client *bigquery.Client
}

func (r *realClient) Query(q string, location string) (RowIterator, error) {
	query := r.client.Query(q)
	if location != "" {
		query.Location = location
	}
	it, err := query.Read(context.Background())
	if err != nil {
		return nil, err
	}
	return &realRowIterator{it: it}, nil
}

func (r *realClient) Close() error {
	return r.client.Close()
}

// BigQueryAdapter executes SQL queries against Google Cloud BigQuery
// and materialises the results into CSV files.
type BigQueryAdapter struct {
	cfg    config.SourceConfig
	log    *zap.Logger
	client Client // optional injected client (for tests)
}

// New creates a new BigQueryAdapter.
func New(cfg config.SourceConfig, log *zap.Logger) *BigQueryAdapter {
	return &BigQueryAdapter{
		cfg: cfg,
		log: log,
	}
}

// NewWithClient creates an adapter with an existing client (used in testing).
func NewWithClient(cfg config.SourceConfig, log *zap.Logger, client Client) *BigQueryAdapter {
	return &BigQueryAdapter{
		cfg:    cfg,
		log:    log,
		client: client,
	}
}

func (a *BigQueryAdapter) ID() string { return a.cfg.ID }

// resolveProjectID determines the GCP project ID to use.
func (a *BigQueryAdapter) resolveProjectID() (string, error) {
	if a.cfg.BigQuery != nil && a.cfg.BigQuery.ProjectID != "" {
		return a.cfg.BigQuery.ProjectID, nil
	}
	if p := os.Getenv("GOOGLE_CLOUD_PROJECT"); p != "" {
		return p, nil
	}
	if p := os.Getenv("GCLOUD_PROJECT"); p != "" {
		return p, nil
	}
	if p := os.Getenv("CLOUDSDK_CORE_PROJECT"); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("source %q: bigquery project_id is required (specify in vfs.yaml or via GOOGLE_CLOUD_PROJECT env)", a.cfg.ID)
}

// getClient instantiates or returns the BigQuery client.
func (a *BigQueryAdapter) getClient(ctx context.Context) (Client, error) {
	if a.client != nil {
		return a.client, nil
	}

	projectID, err := a.resolveProjectID()
	if err != nil {
		return nil, err
	}

	var opts []option.ClientOption
	if a.cfg.BigQuery != nil && a.cfg.BigQuery.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(a.cfg.BigQuery.CredentialsFile))
	}

	bqClient, err := bigquery.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("bigquery[%s]: init client for project %q: %w", a.cfg.ID, projectID, err)
	}

	return &realClient{client: bqClient}, nil
}

// Validate checks that the project ID is available and client initializes.
func (a *BigQueryAdapter) Validate(ctx context.Context) error {
	client, err := a.getClient(ctx)
	if err != nil {
		return err
	}
	if a.client == nil {
		_ = client.Close()
	}
	return nil
}

// Fetch executes the BigQuery SQL query and streams the rows to a CSV file.
func (a *BigQueryAdapter) Fetch(ctx context.Context, outDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{
		SourceID: a.cfg.ID,
		Status:   "start",
		Message:  "connecting to Google Cloud BigQuery",
	}

	client, err := a.getClient(ctx)
	if err != nil {
		return fmt.Errorf("bigquery[%s]: %w", a.cfg.ID, err)
	}
	if a.client == nil {
		defer client.Close()
	}

	location := ""
	if a.cfg.BigQuery != nil {
		location = a.cfg.BigQuery.Location
	}

	it, err := client.Query(a.cfg.Query, location)
	if err != nil {
		return fmt.Errorf("bigquery[%s]: query read: %w", a.cfg.ID, err)
	}

	outPath := filepath.Join(outDir, a.cfg.Materialize.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("bigquery[%s]: mkdir: %w", a.cfg.ID, err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("bigquery[%s]: create file %q: %w", a.cfg.ID, outPath, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)

	headerWritten := false
	count := 0

	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("bigquery[%s]: next row %d: %w", a.cfg.ID, count, err)
		}

		if !headerWritten {
			schema := it.Schema()
			header := make([]string, len(schema))
			for i, field := range schema {
				header[i] = field.Name
			}
			if err := w.Write(header); err != nil {
				return fmt.Errorf("bigquery[%s]: write header: %w", a.cfg.ID, err)
			}
			headerWritten = true
		}

		record := make([]string, len(row))
		for i, v := range row {
			record[i] = ValueToString(v)
		}

		if err := w.Write(record); err != nil {
			return fmt.Errorf("bigquery[%s]: write row %d: %w", a.cfg.ID, count, err)
		}

		count++
		if count%1000 == 0 {
			w.Flush()
			progress <- adapters.Event{
				SourceID: a.cfg.ID,
				Status:   "progress",
				Rows:     count,
			}
		}
	}

	// In case query returned 0 rows, still write header if schema is available
	if !headerWritten {
		schema := it.Schema()
		if len(schema) > 0 {
			header := make([]string, len(schema))
			for i, field := range schema {
				header[i] = field.Name
			}
			_ = w.Write(header)
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("bigquery[%s]: csv flush: %w", a.cfg.ID, err)
	}

	progress <- adapters.Event{
		SourceID: a.cfg.ID,
		Status:   "done",
		Message:  fmt.Sprintf("wrote %d rows to %s", count, a.cfg.Materialize.Output),
		Rows:     count,
	}
	return nil
}

// ValueToString converts BigQuery row values into CSV safe strings.
func ValueToString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case time.Time:
		return t.Format(time.RFC3339)
	case civil.Date:
		return t.String()
	case civil.DateTime:
		return t.String()
	case civil.Time:
		return t.String()
	case bigquery.NullString:
		if !t.Valid {
			return ""
		}
		return t.StringVal
	case bigquery.NullInt64:
		if !t.Valid {
			return ""
		}
		return strconv.FormatInt(t.Int64, 10)
	case bigquery.NullFloat64:
		if !t.Valid {
			return ""
		}
		return strconv.FormatFloat(t.Float64, 'f', -1, 64)
	case bigquery.NullBool:
		if !t.Valid {
			return ""
		}
		return strconv.FormatBool(t.Bool)
	case bigquery.NullTimestamp:
		if !t.Valid {
			return ""
		}
		return t.Timestamp.Format(time.RFC3339)
	case bigquery.NullDate:
		if !t.Valid {
			return ""
		}
		return t.Date.String()
	case bigquery.NullDateTime:
		if !t.Valid {
			return ""
		}
		return t.DateTime.String()
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}
