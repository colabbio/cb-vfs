// Package orthanc implements a MedVFS adapter for DICOMweb-compatible servers.
// Uses QIDO-RS for study discovery, WADO-RS for metadata retrieval.
// Works with Orthanc (local RWE), hospital PACS, or GCP Healthcare DICOM Store.
package orthanc

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
	"strings"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/secrets"
	"go.uber.org/zap"
)

const defaultDICOMwebPath = "/dicom-web"

// DICOM tags (hex, uppercase, no separators — DICOMweb JSON model format)
const (
	tagStudyInstanceUID       = "0020000D"
	tagSeriesInstanceUID      = "0020000E"
	tagPatientID              = "00100020"
	tagPatientName            = "00100010"
	tagStudyDate              = "00080020"
	tagStudyTime              = "00080030"
	tagModality               = "00080060"
	tagModalitiesInStudy      = "00080061"
	tagStudyDescription       = "00081030"
	tagSeriesDescription      = "0008103E"
	tagSeriesNumber           = "00200011"
	tagNumSeriesInstances     = "00201209"
	tagNumberOfStudyRelSeries = "00201206"
	tagInstitutionName        = "00080080"
	tagAccessionNumber        = "00080050"
)

// Adapter fetches DICOM study/series metadata from a DICOMweb server.
type Adapter struct {
	cfg    config.SourceConfig
	client *http.Client
	log    *zap.Logger
	base   string
}

// dicomAttr represents one DICOM attribute in DICOMweb JSON model.
type dicomAttr struct {
	VR    string   `json:"vr"`
	Value []string `json:"Value"`
}

// dicomStudy is a DICOM JSON model object (map of tag -> attribute).
type dicomStudy map[string]dicomAttr

func (d dicomStudy) str(tag string) string {
	if a, ok := d[tag]; ok && len(a.Value) > 0 {
		return a.Value[0]
	}
	return ""
}

// New creates a new Orthanc/DICOMweb adapter.
func New(cfg config.SourceConfig, log *zap.Logger) *Adapter {
	webPath := defaultDICOMwebPath
	if cfg.Orthanc != nil && cfg.Orthanc.DICOMwebPath != "" {
		webPath = cfg.Orthanc.DICOMwebPath
	}
	client, err := secrets.BuildAuthenticatedHTTPClient(cfg.Endpoint, cfg.Auth, 120*time.Second)
	if err != nil || client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Adapter{
		cfg:    cfg,
		log:    log,
		base:   strings.TrimRight(cfg.Endpoint, "/") + webPath,
		client: client,
	}
}

// ID implements adapters.Adapter.
func (a *Adapter) ID() string { return a.cfg.ID }

// Validate checks connectivity to the DICOMweb endpoint.
func (a *Adapter) Validate(ctx context.Context) error {
	if a.cfg.Endpoint == "" {
		return fmt.Errorf("orthanc adapter %q: endpoint is required", a.cfg.ID)
	}
	if a.cfg.Orthanc == nil {
		return fmt.Errorf("orthanc adapter %q: orthanc: block is required", a.cfg.ID)
	}
	if a.cfg.Auth != nil && a.cfg.Auth.Type != "" && a.cfg.Auth.Type != "none" {
		client, err := secrets.BuildAuthenticatedHTTPClient(a.cfg.Endpoint, a.cfg.Auth, 120*time.Second)
		if err != nil {
			return fmt.Errorf("orthanc adapter %q: auth: %w", a.cfg.ID, err)
		}
		a.client = client
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/studies?limit=1", nil)
	a.setAuth(req)
	req.Header.Set("Accept", "application/dicom+json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("orthanc adapter %q: cannot reach DICOMweb endpoint: %w", a.cfg.ID, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("orthanc adapter %q: QIDO-RS ping returned %d", a.cfg.ID, resp.StatusCode)
	}
	return nil
}

// Fetch queries DICOM studies and materialises the configured extracts.
func (a *Adapter) Fetch(ctx context.Context, outputDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "start", Message: "querying DICOMweb QIDO-RS"}

	studies, err := a.queryStudies(ctx)
	if err != nil {
		return fmt.Errorf("orthanc adapter %q: QIDO-RS: %w", a.cfg.ID, err)
	}
	a.log.Info("DICOM studies found", zap.String("source", a.cfg.ID), zap.Int("count", len(studies)))

	if q := a.cfg.Orthanc; q.MaxStudies > 0 && len(studies) > q.MaxStudies {
		studies = studies[:q.MaxStudies]
	}

	extracts := a.cfg.Orthanc.Extracts
	if len(extracts) == 0 {
		extracts = []string{"metadata_csv"}
	}
	for _, extract := range extracts {
		switch extract {
		case "metadata_csv":
			if err := a.writeMetadataCSV(studies, outputDir); err != nil {
				return fmt.Errorf("orthanc adapter %q: metadata_csv: %w", a.cfg.ID, err)
			}
		case "series_manifest":
			if err := a.writeSeriesManifest(ctx, studies, outputDir); err != nil {
				return fmt.Errorf("orthanc adapter %q: series_manifest: %w", a.cfg.ID, err)
			}
		case "thumbnail":
			if err := a.writeThumbnails(ctx, studies, outputDir); err != nil {
				return fmt.Errorf("orthanc adapter %q: thumbnail: %w", a.cfg.ID, err)
			}
		default:
			a.log.Warn("Unknown extract type, skipping", zap.String("extract", extract))
		}
	}
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "done", Message: "DICOM extracts complete", Rows: len(studies)}
	return nil
}

func (a *Adapter) queryStudies(ctx context.Context) ([]dicomStudy, error) {
	params := url.Values{"includefield": {"all"}}
	if f := a.cfg.Orthanc.StudyFilter; f != nil {
		if f.Modality != "" {
			params.Set("ModalitiesInStudy", f.Modality)
		}
		if f.PatientID != "" {
			params.Set("PatientID", f.PatientID)
		}
		if f.StudyDateFrom != "" || f.StudyDateTo != "" {
			from := f.StudyDateFrom
			if from == "" {
				from = "19000101"
			}
			to := f.StudyDateTo
			if to == "" {
				to = "99991231"
			}
			params.Set("StudyDate", from+"-"+to)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/studies?"+params.Encode(), nil)
	a.setAuth(req)
	req.Header.Set("Accept", "application/dicom+json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("QIDO-RS studies: status %d: %s", resp.StatusCode, body)
	}
	var studies []dicomStudy
	return studies, json.NewDecoder(resp.Body).Decode(&studies)
}

func (a *Adapter) writeMetadataCSV(studies []dicomStudy, outputDir string) error {
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
	_ = w.Write([]string{
		"study_instance_uid", "patient_id", "patient_name",
		"study_date", "study_time", "modality", "modalities_in_study",
		"study_description", "num_series", "institution_name",
		"accession_number", "wado_rs_url",
	})
	for _, s := range studies {
		uid := s.str(tagStudyInstanceUID)
		_ = w.Write([]string{
			uid, s.str(tagPatientID), s.str(tagPatientName),
			s.str(tagStudyDate), s.str(tagStudyTime),
			s.str(tagModality), s.str(tagModalitiesInStudy),
			s.str(tagStudyDescription), s.str(tagNumberOfStudyRelSeries),
			s.str(tagInstitutionName), s.str(tagAccessionNumber),
			a.base + "/studies/" + uid,
		})
	}
	a.log.Info("DICOM metadata CSV written", zap.String("path", outPath), zap.Int("studies", len(studies)))
	return nil
}

func (a *Adapter) writeSeriesManifest(ctx context.Context, studies []dicomStudy, outputDir string) error {
	outPath := filepath.Join(outputDir, "series_manifest.csv")
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{
		"study_instance_uid", "patient_id", "modality",
		"series_instance_uid", "series_number", "series_description",
		"num_instances", "wado_rs_series_url", "wado_rs_metadata_url",
	})
	for _, study := range studies {
		studyUID := study.str(tagStudyInstanceUID)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/studies/%s/series?includefield=all", a.base, studyUID), nil)
		a.setAuth(req)
		req.Header.Set("Accept", "application/dicom+json")
		resp, err := a.client.Do(req)
		if err != nil {
			a.log.Warn("Series list failed", zap.String("study", studyUID), zap.Error(err))
			continue
		}
		if resp.StatusCode == http.StatusOK {
			var seriesList []dicomStudy
			if json.NewDecoder(resp.Body).Decode(&seriesList) == nil {
				for _, s := range seriesList {
					suid := s.str(tagSeriesInstanceUID)
					_ = w.Write([]string{
						studyUID, study.str(tagPatientID), study.str(tagModality),
						suid, s.str(tagSeriesNumber), s.str(tagSeriesDescription),
						s.str(tagNumSeriesInstances),
						fmt.Sprintf("%s/studies/%s/series/%s", a.base, studyUID, suid),
						fmt.Sprintf("%s/studies/%s/series/%s/metadata", a.base, studyUID, suid),
					})
				}
			}
		}
		resp.Body.Close()
	}
	a.log.Info("DICOM series manifest written", zap.String("path", outPath))
	return nil
}

func (a *Adapter) writeThumbnails(ctx context.Context, studies []dicomStudy, outputDir string) error {
	thumbDir := filepath.Join(outputDir, "thumbnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return err
	}
	for _, study := range studies {
		uid := study.str(tagStudyInstanceUID)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/studies/%s/thumbnail", a.base, uid), nil)
		a.setAuth(req)
		resp, err := a.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			a.log.Warn("Thumbnail unavailable", zap.String("study", uid))
			continue
		}
		func() {
			defer resp.Body.Close()
			out, err := os.Create(filepath.Join(thumbDir, uid+".jpg"))
			if err != nil {
				return
			}
			defer out.Close()
			io.Copy(out, resp.Body) //nolint:errcheck
		}()
	}
	a.log.Info("DICOM thumbnails done", zap.String("dir", thumbDir))
	return nil
}

func (a *Adapter) setAuth(req *http.Request) {
	if a.cfg.Auth == nil || a.cfg.Auth.Type == "none" {
		return
	}
	// If client uses AuthenticatedRoundTripper, it handles auth automatically.
	// Keep explicit header fallback when a custom http.Client was injected in tests.
	if _, ok := a.client.Transport.(*secrets.AuthenticatedRoundTripper); ok {
		return
	}
	bundle, err := secrets.ResolveAuthBundle(req.Context(), a.cfg.Auth)
	if err != nil {
		return
	}
	switch a.cfg.Auth.Type {
	case "basic":
		user := bundle.FirstString("username", "user")
		pass := bundle.FirstString("password", "pass")
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
	case "bearer":
		if tok := bundle.FirstString("token", "access_token", "value"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	case "api_key":
		header := "X-API-Key"
		prefix := ""
		if a.cfg.Auth.APIKey != nil {
			if a.cfg.Auth.APIKey.HeaderName != "" {
				header = a.cfg.Auth.APIKey.HeaderName
			}
			prefix = a.cfg.Auth.APIKey.Prefix
		}
		if keyVal := bundle.FirstString("api_key", "token", "value"); keyVal != "" {
			req.Header.Set(header, prefix+keyVal)
		}
	}
}

// ─── FileProvider (FUSE support) ──────────────────────────────────────────────

// BuildFileTree implements adapters.FileProvider.
// Queries DICOM studies + series + instances via QIDO-RS and produces
// FileMappings with WADO-RS instance URLs as RemoteRef.
//
// Logical path structure:
//
//	{PatientID}/{StudyDate}_{Modality}/{SeriesNumber}/{SOPInstanceUID}.dcm
//
// Strategy:
//   - "download"      → downloads instances via WADO-RS
//   - "symlink_tree"  → not applicable for remote DICOM (no-op, returns fuse_tree)
//   - "fuse_tree"     → returns mappings with WADO-RS URLs for FUSE ReadAt
func (a *Adapter) BuildFileTree(ctx context.Context, outDir string, strategy string) ([]adapters.FileMapping, error) {
	studies, err := a.queryStudies(ctx)
	if err != nil {
		return nil, fmt.Errorf("orthanc adapter %q: QIDO-RS: %w", a.cfg.ID, err)
	}

	if q := a.cfg.Orthanc; q.MaxStudies > 0 && len(studies) > q.MaxStudies {
		studies = studies[:q.MaxStudies]
	}

	var mappings []adapters.FileMapping

	for _, study := range studies {
		studyUID := study.str(tagStudyInstanceUID)
		patientID := study.str(tagPatientID)
		if patientID == "" {
			patientID = "UNKNOWN"
		}
		modality := study.str(tagModality)
		if modality == "" {
			modality = "OT"
		}
		studyDate := study.str(tagStudyDate)
		if studyDate == "" {
			studyDate = "00000000"
		}
		studyDir := studyDate + "_" + modality

		// Query series for this study
		seriesList, err := a.querySeries(ctx, studyUID)
		if err != nil {
			a.log.Warn("Cannot list series", zap.String("study", studyUID), zap.Error(err))
			continue
		}

		for _, series := range seriesList {
			seriesUID := series.str(tagSeriesInstanceUID)
			seriesNum := series.str(tagSeriesNumber)
			if seriesNum == "" {
				seriesNum = "0"
			}
			seriesDir := "series_" + seriesNum

			// Query instances for this series
			instances, err := a.queryInstances(ctx, studyUID, seriesUID)
			if err != nil {
				a.log.Warn("Cannot list instances",
					zap.String("study", studyUID),
					zap.String("series", seriesUID),
					zap.Error(err))
				continue
			}

			for _, inst := range instances {
				sopUID := inst.str("00080018") // SOPInstanceUID
				if sopUID == "" {
					continue
				}

				// WADO-RS instance URL
				wadoURL := fmt.Sprintf("%s/studies/%s/series/%s/instances/%s",
					a.base, studyUID, seriesUID, sopUID)

				logicalPath := filepath.Join(patientID, studyDir, seriesDir, sopUID+".dcm")

				fm := adapters.FileMapping{
					SourceID:    a.cfg.ID,
					LogicalPath: logicalPath,
					RemoteRef:   wadoURL,
					ContentType: "application/dicom",
				}

				switch strategy {
				case "download":
					// Download the instance to outDir
					localPath := filepath.Join(outDir, logicalPath)
					if err := a.downloadInstance(ctx, wadoURL, localPath); err != nil {
						a.log.Warn("Instance download failed",
							zap.String("sop", sopUID), zap.Error(err))
						continue
					}
					if info, err := os.Stat(localPath); err == nil {
						fm.SizeBytes = info.Size()
					}
					fm.LocalPath = localPath

				case "symlink_tree", "fuse_tree":
					// For FUSE: RemoteRef already set, no local file
					// Try to get size from metadata
					fm.SizeBytes = 0 // will be resolved lazily by FUSE GetAttr
				}

				mappings = append(mappings, fm)
			}
		}
	}

	a.log.Info("Orthanc file tree built",
		zap.String("strategy", strategy),
		zap.Int("studies", len(studies)),
		zap.Int("files", len(mappings)))

	return mappings, nil
}

// querySeries fetches series list for a study via QIDO-RS.
func (a *Adapter) querySeries(ctx context.Context, studyUID string) ([]dicomStudy, error) {
	u := fmt.Sprintf("%s/studies/%s/series?includefield=all", a.base, studyUID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	a.setAuth(req)
	req.Header.Set("Accept", "application/dicom+json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("QIDO-RS series: status %d: %s", resp.StatusCode, body)
	}
	var series []dicomStudy
	return series, json.NewDecoder(resp.Body).Decode(&series)
}

// queryInstances fetches instance list for a study/series via QIDO-RS.
func (a *Adapter) queryInstances(ctx context.Context, studyUID, seriesUID string) ([]dicomStudy, error) {
	u := fmt.Sprintf("%s/studies/%s/series/%s/instances?includefield=all",
		a.base, studyUID, seriesUID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	a.setAuth(req)
	req.Header.Set("Accept", "application/dicom+json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("QIDO-RS instances: status %d: %s", resp.StatusCode, body)
	}
	var instances []dicomStudy
	return instances, json.NewDecoder(resp.Body).Decode(&instances)
}

// downloadInstance downloads a single DICOM instance via WADO-RS.
func (a *Adapter) downloadInstance(ctx context.Context, wadoURL, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, wadoURL, nil)
	a.setAuth(req)
	// Request raw DICOM part
	req.Header.Set("Accept", "application/dicom")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WADO-RS download: status %d: %s", resp.StatusCode, body)
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}
