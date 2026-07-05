package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads and validates a vfs.yaml file, returning a parsed VFSConfig.
func Load(path string) (*VFSConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	// Expand environment variables in the YAML before parsing
	expanded := os.ExpandEnv(string(data))

	var cfg VFSConfig
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// validate performs basic semantic validation on the parsed config.
func validate(cfg *VFSConfig) error {
	if cfg.Version == "" {
		return fmt.Errorf("version is required")
	}
	if cfg.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if len(cfg.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}

	validTypes := map[string]bool{
		"omop": true, "fhir": true, "genomics": true, "dicom": true, "files": true, "omero": true, "orthanc": true,
	}
	validStorage := map[string]bool{
		"emptydir": true, "hostpath": true, "pvc": true,
	}

	if !validStorage[cfg.Storage.Type] {
		return fmt.Errorf("storage.type must be one of: emptydir, hostpath, pvc (got %q)", cfg.Storage.Type)
	}

	ids := map[string]bool{}
	for i, s := range cfg.Sources {
		if s.ID == "" {
			return fmt.Errorf("sources[%d]: id is required", i)
		}
		if ids[s.ID] {
			return fmt.Errorf("sources[%d]: duplicate id %q", i, s.ID)
		}
		ids[s.ID] = true
		if !validTypes[s.Type] {
			return fmt.Errorf("source %q: type must be one of: omop, fhir, genomics, dicom (got %q)", s.ID, s.Type)
		}
		if s.Materialize.Output == "" {
			return fmt.Errorf("source %q: materialize.output is required", s.ID)
		}
		if strings.Contains(s.Materialize.Output, "..") {
			return fmt.Errorf("source %q: materialize.output must not contain '..'", s.ID)
		}
	}
	return nil
}
