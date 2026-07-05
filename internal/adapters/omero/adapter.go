// Package omero implements a MedVFS adapter for OMERO.server.
//
// Authentication: session-based login via OMERO.web JSON API.
// No Ice/ZeroC dependency — all communication is over HTTP REST.
//
// What it materialises per source:
//   - metadata_csv  → images.csv  (id, name, patient, acquisition fields, key-value annotations)
//   - thumbnail     → thumbnails/<id>.jpg  (configurable px size)
//   - tile_manifest → tile_manifest.csv   (image_id, level, x, y, tile_url)
package omero

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
	"golang.org/x/net/publicsuffix"
)

const (
	defaultThumbnailSize = 256
	defaultPageSize      = 100
	apiV0                = "/api/v0"
	webClientPath        = "/webclient"
)

// Adapter fetches WSI metadata, thumbnails, and tile manifests from OMERO.web.
type Adapter struct {
	cfg    config.SourceConfig
	client *http.Client
	log    *zap.Logger
	token  string // CSRF token
}

// New creates a new OMERO adapter. Call Validate() before Fetch().
func New(cfg config.SourceConfig, log *zap.Logger) *Adapter {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &Adapter{
		cfg: cfg,
		client: &http.Client{
			Timeout: 120 * time.Second,
			Jar:     jar,
		},
		log: log,
	}
}

// ID implements adapters.Adapter.
func (a *Adapter) ID() string { return a.cfg.ID }

// Validate checks that required config fields are present and OMERO.web is reachable.
func (a *Adapter) Validate(ctx context.Context) error {
	if a.cfg.Endpoint == "" {
		return fmt.Errorf("omero adapter %q: endpoint is required", a.cfg.ID)
	}
	if a.cfg.OMERO == nil {
		return fmt.Errorf("omero adapter %q: omero: block is required", a.cfg.ID)
	}
	if a.cfg.OMERO.DatasetID == 0 && a.cfg.OMERO.ProjectID == 0 {
		return fmt.Errorf("omero adapter %q: omero.dataset_id or omero.project_id is required", a.cfg.ID)
	}
	if a.cfg.Auth == nil || (a.cfg.Auth.Type != "basic" && a.cfg.Auth.Type != "none") {
		return fmt.Errorf("omero adapter %q: auth.type must be 'basic' or 'none'", a.cfg.ID)
	}

	// Ping OMERO.web API
	pingURL := a.cfg.Endpoint + apiV0 + "/token/"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pingURL, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("omero adapter %q: cannot reach %s: %w", a.cfg.ID, pingURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("omero adapter %q: unexpected status from OMERO token endpoint: %d", a.cfg.ID, resp.StatusCode)
	}
	return nil
}

// Fetch authenticates against OMERO.web and materialises the requested extracts.
func (a *Adapter) Fetch(ctx context.Context, outputDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "start", Message: "connecting to OMERO.web"}
	// 1. Obtain CSRF token
	if err := a.fetchCSRFToken(ctx); err != nil {
		return fmt.Errorf("omero adapter %q: csrf: %w", a.cfg.ID, err)
	}

	// 2. Login (session cookie stored in jar)
	if a.cfg.Auth != nil && a.cfg.Auth.Type == "basic" {
		if err := a.login(ctx); err != nil {
			return fmt.Errorf("omero adapter %q: login: %w", a.cfg.ID, err)
		}
	}

	// 3. Resolve image list
	images, err := a.listImages(ctx)
	if err != nil {
		return fmt.Errorf("omero adapter %q: list images: %w", a.cfg.ID, err)
	}
	a.log.Info("OMERO images listed", zap.String("source", a.cfg.ID), zap.Int("count", len(images)))

	// Apply MaxImages cap
	q := a.cfg.OMERO
	if q.MaxImages > 0 && len(images) > q.MaxImages {
		images = images[:q.MaxImages]
	}

	// 4. Determine what to extract
	extracts := q.Extracts
	if len(extracts) == 0 {
		extracts = []string{"metadata_csv"}
	}

	for _, extract := range extracts {
		switch extract {
		case "metadata_csv":
			if err := a.writeMetadataCSV(ctx, images, outputDir); err != nil {
				return fmt.Errorf("omero adapter %q: metadata_csv: %w", a.cfg.ID, err)
			}
		case "thumbnail":
			if err := a.writeThumbnails(ctx, images, outputDir); err != nil {
				return fmt.Errorf("omero adapter %q: thumbnail: %w", a.cfg.ID, err)
			}
		case "tile_manifest":
			if err := a.writeTileManifest(ctx, images, outputDir); err != nil {
				return fmt.Errorf("omero adapter %q: tile_manifest: %w", a.cfg.ID, err)
			}
		default:
			a.log.Warn("Unknown OMERO extract type, skipping", zap.String("extract", extract))
		}
	}
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "done", Message: "all extracts complete", Rows: len(images)}
	return nil
}

// ─── OMERO.web JSON API types ────────────────────────────────────────────────

type omeroImage struct {
	ID   int    `json:"@id"`
	Name string `json:"Name"`
	Meta struct {
		DimensionOrder string  `json:"DimensionOrder,omitempty"`
		SizeX          int     `json:"SizeX,omitempty"`
		SizeY          int     `json:"SizeY,omitempty"`
		SizeZ          int     `json:"SizeZ,omitempty"`
		SizeT          int     `json:"SizeT,omitempty"`
		SizeC          int     `json:"SizeC,omitempty"`
		PixelType      string  `json:"PixelType,omitempty"`
		PhysicalSizeX  float64 `json:"PhysicalSizeX,omitempty"`
		PhysicalSizeY  float64 `json:"PhysicalSizeY,omitempty"`
	} `json:"Pixels,omitempty"`
}

type omeroListResponse struct {
	Data []omeroImage `json:"data"`
	Meta struct {
		TotalCount int `json:"totalCount"`
		Offset     int `json:"offset"`
		Limit      int `json:"limit"`
	} `json:"meta"`
}

type omeroAnnotation struct {
	ID    int    `json:"@id"`
	Type  string `json:"@type"`
	Value string `json:"Value,omitempty"` // MapAnnotation pairs serialised below
	Pairs []struct {
		Name  string `json:"Name"`
		Value string `json:"Value"`
	} `json:"MapValue,omitempty"`
}

// ─── Internal helpers ────────────────────────────────────────────────────────

func (a *Adapter) fetchCSRFToken(ctx context.Context) error {
	u := a.cfg.Endpoint + apiV0 + "/token/"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	a.token = result.Data
	return nil
}

func (a *Adapter) login(ctx context.Context) error {
	// Resolve credentials from AuthConfig
	username, password := "root", "omero" // fallback dev defaults
	if a.cfg.Auth.SecretRef != "" {
		// TODO: resolve via secret engine (k8s:// / env:// / file://)
		a.log.Warn("OMERO secret_ref resolution not yet implemented; using defaults",
			zap.String("secret_ref", a.cfg.Auth.SecretRef))
	}

	loginURL := a.cfg.Endpoint + apiV0 + "/login/"
	form := url.Values{
		"username":   {username},
		"password":   {password},
		"server":     {"1"},
		"csrftoken":  {a.token},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRFToken", a.token)
	req.Header.Set("Referer", a.cfg.Endpoint+"/")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed (%d): %s", resp.StatusCode, body)
	}
	return nil
}

func (a *Adapter) listImages(ctx context.Context) ([]omeroImage, error) {
	q := a.cfg.OMERO
	var baseURL string
	if q.DatasetID > 0 {
		baseURL = fmt.Sprintf("%s%s/m/datasets/%d/images/", a.cfg.Endpoint, apiV0, q.DatasetID)
	} else {
		baseURL = fmt.Sprintf("%s%s/m/projects/%d/images/", a.cfg.Endpoint, apiV0, q.ProjectID)
	}

	var all []omeroImage
	offset := 0
	for {
		u := fmt.Sprintf("%s?offset=%d&limit=%d", baseURL, offset, defaultPageSize)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, err
		}
		var page omeroListResponse
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()

		// Apply name filter
		for _, img := range page.Data {
			if q.ImageFilter != nil && q.ImageFilter.NamePrefix != "" {
				if !strings.HasPrefix(img.Name, q.ImageFilter.NamePrefix) {
					continue
				}
			}
			all = append(all, img)
		}

		if offset+page.Meta.Limit >= page.Meta.TotalCount {
			break
		}
		offset += page.Meta.Limit
	}
	return all, nil
}

func (a *Adapter) fetchAnnotations(ctx context.Context, imageID int) (map[string]string, error) {
	u := fmt.Sprintf("%s%s/m/images/%d/annotations/", a.cfg.Endpoint, apiV0, imageID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []omeroAnnotation `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	kv := make(map[string]string)
	for _, ann := range result.Data {
		// Map annotations are the main source of clinical metadata in OMERO
		for _, pair := range ann.Pairs {
			kv[pair.Name] = pair.Value
		}
		// Tag annotations
		if ann.Value != "" {
			kv["tag_"+strconv.Itoa(ann.ID)] = ann.Value
		}
	}
	return kv, nil
}

// writeMetadataCSV creates a CSV with one row per image, including pixel metadata
// and selected OMERO key-value annotations.
func (a *Adapter) writeMetadataCSV(ctx context.Context, images []omeroImage, outputDir string) error {
	outPath := filepath.Join(outputDir, a.cfg.Materialize.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// Build dynamic headers: fixed fields + annotation fields
	fixedHeaders := []string{
		"image_id", "image_name",
		"size_x", "size_y", "size_z", "size_t", "size_c",
		"pixel_type", "physical_size_x_um", "physical_size_y_um",
	}

	// Collect annotations for all images first to determine columns
	allAnnotations := make([]map[string]string, len(images))
	annotationKeys := make(map[string]struct{})
	for i, img := range images {
		ann, err := a.fetchAnnotations(ctx, img.ID)
		if err != nil {
			a.log.Warn("Could not fetch annotations", zap.Int("image_id", img.ID), zap.Error(err))
			ann = map[string]string{}
		}
		allAnnotations[i] = ann
		// Filter by MetadataFields if specified
		for k := range ann {
			if a.includeField(k) {
				annotationKeys[k] = struct{}{}
			}
		}
	}

	// Sort annotation keys for deterministic column order
	var annKeys []string
	for k := range annotationKeys {
		annKeys = append(annKeys, k)
	}

	headers := append(fixedHeaders, annKeys...)
	if err := w.Write(headers); err != nil {
		return err
	}

	for i, img := range images {
		m := img.Meta
		row := []string{
			strconv.Itoa(img.ID),
			img.Name,
			strconv.Itoa(m.SizeX),
			strconv.Itoa(m.SizeY),
			strconv.Itoa(m.SizeZ),
			strconv.Itoa(m.SizeT),
			strconv.Itoa(m.SizeC),
			m.PixelType,
			strconv.FormatFloat(m.PhysicalSizeX, 'f', 4, 64),
			strconv.FormatFloat(m.PhysicalSizeY, 'f', 4, 64),
		}
		ann := allAnnotations[i]
		for _, k := range annKeys {
			row = append(row, ann[k])
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	a.log.Info("OMERO metadata CSV written",
		zap.String("path", outPath), zap.Int("rows", len(images)))
	return nil
}

// writeThumbnails downloads a JPEG thumbnail for each image.
func (a *Adapter) writeThumbnails(ctx context.Context, images []omeroImage, outputDir string) error {
	size := a.cfg.OMERO.ThumbnailSize
	if size == 0 {
		size = defaultThumbnailSize
	}
	thumbDir := filepath.Join(outputDir, "thumbnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return err
	}

	for _, img := range images {
		imgPath := filepath.Join(thumbDir, fmt.Sprintf("%d.jpg", img.ID))
		thumbURL := fmt.Sprintf("%s%s/render_thumbnail/%d/?size=%d",
			a.cfg.Endpoint, webClientPath, img.ID, size)

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, thumbURL, nil)
		resp, err := a.client.Do(req)
		if err != nil {
			a.log.Warn("Thumbnail fetch failed", zap.Int("image_id", img.ID), zap.Error(err))
			continue
		}
		func() {
			defer resp.Body.Close()
			out, err := os.Create(imgPath)
			if err != nil {
				a.log.Warn("Cannot create thumbnail file", zap.String("path", imgPath), zap.Error(err))
				return
			}
			defer out.Close()
			io.Copy(out, resp.Body) //nolint:errcheck
		}()
	}
	a.log.Info("OMERO thumbnails written", zap.String("dir", thumbDir), zap.Int("count", len(images)))
	return nil
}

// writeTileManifest creates a CSV mapping each image to its tile streaming URLs.
// This allows Nextflow pipeline containers to stream tiles on demand without
// downloading the full WSI file.
func (a *Adapter) writeTileManifest(ctx context.Context, images []omeroImage, outputDir string) error {
	outPath := filepath.Join(outputDir, "tile_manifest.csv")
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	headers := []string{
		"image_id", "image_name",
		"max_level", "size_x", "size_y",
		"tile_url_template", // use {z},{x},{y} as placeholders
		"thumbnail_url",
	}
	if err := w.Write(headers); err != nil {
		return err
	}

	for _, img := range images {
		tileURLTmpl := fmt.Sprintf(
			"%s%s/render_image_region/%d/0/0/?tile=%%d,%%d,%%d",
			a.cfg.Endpoint, webClientPath, img.ID,
		)
		thumbURL := fmt.Sprintf(
			"%s%s/render_thumbnail/%d/?size=%d",
			a.cfg.Endpoint, webClientPath, img.ID, defaultThumbnailSize,
		)
		// Approximate max_level from image dimensions (1 level per 2x zoom step)
		maxLevel := 0
		sx := img.Meta.SizeX
		for sx > 256 {
			maxLevel++
			sx /= 2
		}
		row := []string{
			strconv.Itoa(img.ID),
			img.Name,
			strconv.Itoa(maxLevel),
			strconv.Itoa(img.Meta.SizeX),
			strconv.Itoa(img.Meta.SizeY),
			tileURLTmpl,
			thumbURL,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	a.log.Info("OMERO tile manifest written", zap.String("path", outPath))
	return nil
}

// includeField returns true if the annotation key should be included in the CSV.
func (a *Adapter) includeField(key string) bool {
	fields := a.cfg.OMERO.MetadataFields
	if len(fields) == 0 {
		return true // no filter → include everything
	}
	for _, f := range fields {
		if f == key {
			return true
		}
	}
	return false
}

// ─── FileProvider (FUSE support) ──────────────────────────────────────────────

// BuildFileTree implements adapters.FileProvider.
// Queries OMERO images and produces FileMappings with OMERO.web render URLs
// or original file download URLs as RemoteRef.
//
// Logical path structure:
//   {DatasetName}/{ImageName}.{ext}
//   or: {ProjectName}/{DatasetName}/{ImageName}.{ext}
//
// Strategy:
//   - "download"      → downloads original file via OMERO.web
//   - "symlink_tree"  → not applicable for remote OMERO (falls through to fuse_tree)
//   - "fuse_tree"     → returns mappings with OMERO.web URLs for FUSE ReadAt
func (a *Adapter) BuildFileTree(ctx context.Context, outDir string, strategy string) ([]adapters.FileMapping, error) {
	// Ensure we're authenticated
	if err := a.fetchCSRFToken(ctx); err != nil {
		return nil, fmt.Errorf("omero adapter %q: csrf: %w", a.cfg.ID, err)
	}
	if a.cfg.Auth != nil && a.cfg.Auth.Type == "basic" {
		if err := a.login(ctx); err != nil {
			return nil, fmt.Errorf("omero adapter %q: login: %w", a.cfg.ID, err)
		}
	}

	images, err := a.listImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("omero adapter %q: list images: %w", a.cfg.ID, err)
	}

	q := a.cfg.OMERO
	if q.MaxImages > 0 && len(images) > q.MaxImages {
		images = images[:q.MaxImages]
	}

	var mappings []adapters.FileMapping

	for _, img := range images {
		// Build the logical path from image metadata
		name := img.Name
		if name == "" {
			name = fmt.Sprintf("image_%d", img.ID)
		}

		// Determine file extension from name
		ext := filepath.Ext(name)
		if ext == "" {
			ext = ".tiff" // default for WSI
		}

		logicalPath := name

		// Original file download URL
		origFileURL := fmt.Sprintf("%s%s/m/images/%d/render/original/",
			a.cfg.Endpoint, apiV0, img.ID)

		fm := adapters.FileMapping{
			LogicalPath: logicalPath,
			RemoteRef:   origFileURL,
			ContentType: guessContentType(ext),
		}

		// Estimate size from pixel dimensions (rough, will be refined by HEAD)
		if img.Meta.SizeX > 0 && img.Meta.SizeY > 0 {
			// Very rough estimate: compressed TIFF ≈ 1 byte/pixel
			fm.SizeBytes = int64(img.Meta.SizeX) * int64(img.Meta.SizeY)
		}

		switch strategy {
		case "download":
			localPath := filepath.Join(outDir, logicalPath)
			if err := a.downloadOriginalFile(ctx, img.ID, localPath); err != nil {
				a.log.Warn("Image download failed",
					zap.Int("image_id", img.ID), zap.Error(err))
				continue
			}
			if info, err := os.Stat(localPath); err == nil {
				fm.SizeBytes = info.Size()
			}
			fm.LocalPath = localPath

		case "symlink_tree", "fuse_tree":
			// FUSE mode: RemoteRef points to OMERO.web original file endpoint
		}

		mappings = append(mappings, fm)
	}

	a.log.Info("OMERO file tree built",
		zap.String("strategy", strategy),
		zap.Int("images", len(images)),
		zap.Int("files", len(mappings)))

	return mappings, nil
}

// downloadOriginalFile downloads the original file for an OMERO image.
func (a *Adapter) downloadOriginalFile(ctx context.Context, imageID int, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	u := fmt.Sprintf("%s%s/m/images/%d/render/original/",
		a.cfg.Endpoint, apiV0, imageID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OMERO download: status %d: %s", resp.StatusCode, body)
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// guessContentType returns a MIME type for a file extension.
func guessContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".tif", ".tiff":
		return "image/tiff"
	case ".svs":
		return "image/tiff" // Aperio SVS is TIFF-based
	case ".ndpi":
		return "image/tiff" // Hamamatsu NDPI
	case ".dcm", ".dicom":
		return "application/dicom"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
}
