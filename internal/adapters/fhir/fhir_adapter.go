package fhir

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/secrets"
)

const defaultPageSize = 100

// FHIRAdapter fetches resources from a FHIR R4 server and writes CSV files.
type FHIRAdapter struct {
	cfg    config.SourceConfig
	client *http.Client
	token  string // resolved bearer token (may be empty for none auth)
}

// New creates a FHIRAdapter. Call Validate before Fetch.
func New(cfg config.SourceConfig) *FHIRAdapter {
	return &FHIRAdapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (a *FHIRAdapter) ID() string { return a.cfg.ID }

// Validate checks connectivity with a metadata ping to the FHIR server.
func (a *FHIRAdapter) Validate(ctx context.Context) error {
	if err := a.resolveAuth(ctx); err != nil {
		return fmt.Errorf("fhir[%s]: auth: %w", a.cfg.ID, err)
	}
	// FHIR capability statement ping
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(a.cfg.Endpoint, "/")+"/metadata", nil)
	a.setHeaders(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("fhir[%s]: ping %s/metadata: %w", a.cfg.ID, a.cfg.Endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fhir[%s]: metadata returned HTTP %d", a.cfg.ID, resp.StatusCode)
	}
	return nil
}

// Fetch performs paginated FHIR search and writes each resource as a CSV row.
func (a *FHIRAdapter) Fetch(ctx context.Context, outDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "start",
		Message: fmt.Sprintf("querying FHIR %s/%s", a.cfg.Endpoint, a.cfg.FHIR.Resource)}

	if err := a.resolveAuth(ctx); err != nil {
		return fmt.Errorf("fhir[%s]: auth: %w", a.cfg.ID, err)
	}

	outPath := filepath.Join(outDir, a.cfg.Materialize.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("fhir[%s]: mkdir: %w", a.cfg.ID, err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("fhir[%s]: create %q: %w", a.cfg.ID, outPath, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	rawFields := a.cfg.FHIR.Fields
	headers := make([]string, len(rawFields))
	paths := make([]string, len(rawFields))
	for i, f := range rawFields {
		parts := strings.SplitN(f, " as ", 2)
		paths[i] = strings.TrimSpace(parts[0])
		if len(parts) == 2 {
			headers[i] = strings.TrimSpace(parts[1])
		} else {
			headers[i] = paths[i]
		}
	}
	headerWritten := false
	count := 0

	nextURL := a.buildSearchURL()

	for nextURL != "" {
		bundle, err := a.fetchBundle(ctx, nextURL)
		if err != nil {
			return fmt.Errorf("fhir[%s]: fetch page: %w", a.cfg.ID, err)
		}

		for _, entry := range bundle.Entry {
			var resource map[string]any
			if err := json.Unmarshal(entry.Resource, &resource); err != nil {
				continue
			}

			if len(paths) == 0 {
				// No fields configured: emit full resource as single JSON column
				if !headerWritten {
					_ = w.Write([]string{"resource_json"})
					headerWritten = true
				}
				raw, _ := json.Marshal(resource)
				_ = w.Write([]string{string(raw)})
			} else {
				if !headerWritten {
					_ = w.Write(headers)
					headerWritten = true
				}
				row := make([]string, len(paths))
				for i, p := range paths {
					row[i] = extractField(resource, p)
				}
				_ = w.Write(row)
			}
			count++
		}

		w.Flush()
		nextURL = bundle.nextLink()

		if count%500 == 0 && count > 0 {
			progress <- adapters.Event{SourceID: a.cfg.ID, Status: "progress", Rows: count}
		}
	}

	if err := w.Error(); err != nil {
		return fmt.Errorf("fhir[%s]: csv flush: %w", a.cfg.ID, err)
	}

	progress <- adapters.Event{
		SourceID: a.cfg.ID,
		Status:   "done",
		Message:  fmt.Sprintf("wrote %d resources to %s", count, a.cfg.Materialize.Output),
		Rows:     count,
	}
	return nil
}

// ─── FHIR Bundle types ────────────────────────────────────────────────────────

type bundle struct {
	Total int           `json:"total"`
	Link  []bundleLink  `json:"link"`
	Entry []bundleEntry `json:"entry"`
}

type bundleLink struct {
	Relation string `json:"relation"`
	URL      string `json:"url"`
}

type bundleEntry struct {
	Resource json.RawMessage `json:"resource"`
}

func (b *bundle) nextLink() string {
	for _, l := range b.Link {
		if l.Relation == "next" {
			return l.URL
		}
	}
	return ""
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// buildSearchURL constructs the initial FHIR search URL with query parameters.
func (a *FHIRAdapter) buildSearchURL() string {
	base := strings.TrimRight(a.cfg.Endpoint, "/")
	u, _ := url.Parse(fmt.Sprintf("%s/%s", base, a.cfg.FHIR.Resource))
	q := url.Values{}

	pageSize := a.cfg.FHIR.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	q.Set("_count", strconv.Itoa(pageSize))

	for k, v := range a.cfg.FHIR.Params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// fetchBundle GETs a FHIR Bundle from the given URL.
func (a *FHIRAdapter) fetchBundle(ctx context.Context, rawURL string) (*bundle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	a.setHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var b bundle
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, fmt.Errorf("decode bundle: %w", err)
	}
	return &b, nil
}

// setHeaders adds Accept and Authorization headers to a request.
func (a *FHIRAdapter) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/fhir+json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
}

// resolveAuth initializes the authenticated HTTP client and populates a.token
// using the unified SecretBundle & AuthenticatedRoundTripper engine (CB-SPEC-CRED-022).
func (a *FHIRAdapter) resolveAuth(ctx context.Context) error {
	if a.cfg.Auth == nil || a.cfg.Auth.Type == "" || a.cfg.Auth.Type == "none" {
		return nil
	}
	switch a.cfg.Auth.Type {
	case "bearer":
		if a.cfg.Auth.Token != "" {
			a.token = a.cfg.Auth.Token
			return nil
		}
		if a.cfg.Auth.SecretRef != "" {
			tok, err := secrets.Resolve(ctx, a.cfg.Auth.SecretRef)
			if err != nil {
				return fmt.Errorf("bearer resolve secret_ref: %w", err)
			}
			a.token = tok
			return nil
		}
		return fmt.Errorf("bearer: token or secret_ref required")

	case "smart_on_fhir":
		tok, err := a.fetchSMARTToken(ctx)
		if err != nil {
			return fmt.Errorf("smart_on_fhir: %w", err)
		}
		a.token = tok
		return nil

	case "basic", "api_key", "oauth2_client_credentials", "smart_backend_jwt", "mtls", "mtls_oauth2":
		client, err := secrets.BuildAuthenticatedHTTPClient(a.cfg.Endpoint, a.cfg.Auth, 60*time.Second)
		if err != nil {
			return err
		}
		a.client = client
		return nil

	default:
		return fmt.Errorf("unsupported auth type %q", a.cfg.Auth.Type)
	}
}

// fetchSMARTToken performs an OAuth2 client_credentials grant for SMART-on-FHIR.
func (a *FHIRAdapter) fetchSMARTToken(ctx context.Context) (string, error) {
	auth := a.cfg.Auth
	bundle, err := secrets.ResolveAuthBundle(ctx, auth)
	if err != nil {
		return "", err
	}

	tokenURL := auth.TokenURL
	if tokenURL == "" && auth.OAuth2 != nil {
		tokenURL = auth.OAuth2.TokenURL
	}
	if tokenURL == "" {
		tokenURL = bundle.FirstString("token_url")
	}
	if tokenURL == "" {
		return "", fmt.Errorf("token_url is required for smart_on_fhir auth")
	}

	clientID := bundle.FirstString("client_id")
	clientSecret := bundle.FirstString("client_secret", "secret", "value")
	if clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("client_id and client_secret are required for smart_on_fhir auth")
	}

	scope := auth.Scope
	if scope == "" {
		scope = bundle.FirstString("scope")
	}
	if scope == "" {
		scope = "system/*.read"
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"scope":         {scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token endpoint HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	return tok.AccessToken, nil
}

// ─── Field extractor ──────────────────────────────────────────────────────────

// extractField navigates a FHIR resource map using dot-path notation.
// Examples: "id", "name.0.family", "name.0.given.0", "birthDate"
func extractField(resource map[string]any, path string) string {
	parts := strings.SplitN(path, ".", 2)
	key := parts[0]

	val, ok := resource[key]
	if !ok {
		return ""
	}

	if len(parts) == 1 {
		return anyToString(val)
	}

	rest := parts[1]

	switch typed := val.(type) {
	case map[string]any:
		return extractField(typed, rest)
	case []any:
		// rest starts with a numeric index
		idx, rem, hasRem := strings.Cut(rest, ".")
		i, err := strconv.Atoi(idx)
		if err != nil || i >= len(typed) {
			return ""
		}
		elem := typed[i]
		if !hasRem {
			return anyToString(elem)
		}
		if m, ok := elem.(map[string]any); ok {
			return extractField(m, rem)
		}
		return ""
	default:
		return anyToString(typed)
	}
}

func anyToString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
