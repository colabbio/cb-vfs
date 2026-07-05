package main

import (
	"context"
	"fmt"
	"os"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/adapters/dicom"
	"github.com/colabbio/medvfs/internal/adapters/fhir"
	filesadapter "github.com/colabbio/medvfs/internal/adapters/files"
	"github.com/colabbio/medvfs/internal/adapters/genomics"
	"github.com/colabbio/medvfs/internal/adapters/omero"
	"github.com/colabbio/medvfs/internal/adapters/omop"
	"github.com/colabbio/medvfs/internal/adapters/orthanc"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/secrets"
	"github.com/colabbio/medvfs/internal/serve"
	"github.com/colabbio/medvfs/internal/setup"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	cfgPath string
	log     *zap.Logger
)

func main() {
	var err error
	log, err = zap.NewDevelopment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	root := &cobra.Command{
		Use:   "medvfs",
		Short: "MedVFS — Medical Virtual File System for ML pipelines",
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "vfs.yaml", "path to vfs.yaml")

	root.AddCommand(cmdSetup(), cmdValidate(), cmdVersion(), cmdServe())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// cmdSetup runs the data materialisation phase.
func cmdSetup() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Materialise data sources into the output directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			outDir, err := resolveOutputDir(cmd.Context(), cfg)
			if err != nil {
				return fmt.Errorf("resolve output dir: %w", err)
			}

			log.Info("starting setup phase",
				zap.String("name", cfg.Metadata.Name),
				zap.String("outDir", outDir),
				zap.Int("sources", len(cfg.Sources)),
			)

			engine := setup.New(cfg, outDir, log, buildAdapterFactory(cmd.Context()))
			results, err := engine.Run(cmd.Context())
			if err != nil {
				return err
			}

			log.Info("setup complete", zap.Int("sources_done", len(results)))
			return nil
		},
	}
}

// cmdValidate validates the vfs.yaml without fetching data.
func cmdValidate() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate a vfs.yaml configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			fmt.Printf("✅ %q is valid — %d source(s) defined\n", cfgPath, len(cfg.Sources))
			return nil
		},
	}
}

func cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print MedVFS version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("medvfs v0.1.0 (colabbio)")
		},
	}
}

// resolveOutputDir computes the output directory based on storage config.
func resolveOutputDir(ctx context.Context, cfg *config.VFSConfig) (string, error) {
	// Direct override for dev/test
	if cfg.Storage.OutputDir != "" {
		if err := os.MkdirAll(cfg.Storage.OutputDir, 0o755); err != nil {
			return "", err
		}
		return cfg.Storage.OutputDir, nil
	}

	var base string
	switch cfg.Storage.Type {
	case "emptydir":
		base = os.TempDir()
	case "hostpath":
		base = cfg.Storage.HostPath
	case "pvc":
		// In K8s, PVC is mounted at a path — use the pvc_name as a hint
		base = fmt.Sprintf("/mnt/colabbio/%s", cfg.Storage.PVCName)
	default:
		return "", fmt.Errorf("unknown storage type: %s", cfg.Storage.Type)
	}

	outDir := base
	if cfg.Storage.SubPath != "" {
		outDir = fmt.Sprintf("%s/%s", base, cfg.Storage.SubPath)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", outDir, err)
	}
	return outDir, nil
}

// buildAdapterFactory returns a factory function that creates adapters per source type.
func buildAdapterFactory(ctx context.Context) setup.AdapterFactory {
	return func(src config.SourceConfig) (adapters.Adapter, error) {
		switch src.Type {
		case "omop":
			dsn, err := resolveDSN(ctx, src)
			if err != nil {
				return nil, err
			}
			return omop.New(src, dsn), nil
		case "fhir":
			if src.Endpoint == "" {
				return nil, fmt.Errorf("source %q: endpoint is required for type fhir", src.ID)
			}
			if src.FHIR == nil || src.FHIR.Resource == "" {
				return nil, fmt.Errorf("source %q: fhir.resource is required for type fhir", src.ID)
			}
			return fhir.New(src), nil
		case "omero":
			if src.Endpoint == "" {
				return nil, fmt.Errorf("source %q: endpoint is required for type omero", src.ID)
			}
			if src.OMERO == nil {
				return nil, fmt.Errorf("source %q: omero: block is required", src.ID)
			}
			return omero.New(src, log), nil
		case "orthanc":
			if src.Endpoint == "" {
				return nil, fmt.Errorf("source %q: endpoint is required for type orthanc", src.ID)
			}
			if src.Orthanc == nil {
				return nil, fmt.Errorf("source %q: orthanc: block is required", src.ID)
			}
			return orthanc.New(src, log), nil
		case "genomics":
			if src.Genomics == nil {
				return nil, fmt.Errorf("source %q: genomics: block is required", src.ID)
			}
			return genomics.New(src, log)
		case "dicom":
			if src.DICOM == nil {
				return nil, fmt.Errorf("source %q: dicom: block is required", src.ID)
			}
			return dicom.New(src, log), nil
		case "files":
			if src.Files == nil {
				return nil, fmt.Errorf("source %q: files: block is required", src.ID)
			}
			return filesadapter.New(src, log), nil
		default:
			return nil, fmt.Errorf("adapter type %q not yet implemented", src.Type)
		}
	}
}

// resolveDSN resolves the PostgreSQL DSN for an OMOP source.
func resolveDSN(ctx context.Context, src config.SourceConfig) (string, error) {
	if src.DSN != "" {
		return src.DSN, nil // literal DSN (dev only)
	}
	if src.DSNSecret != "" {
		return secrets.Resolve(ctx, src.DSNSecret)
	}
	return "", fmt.Errorf("source %q: either dsn or dsn_secret is required", src.ID)
}

// cmdServe runs the persistent daemon mode with FUSE mount and HTTP API.
func cmdServe() *cobra.Command {
	var (
		fuseEnabled bool
		port        int
		cacheDir    string
		mountPoint  string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run MedVFS as a persistent daemon with FUSE mount and HTTP API",
		Long: `Serve mode keeps MedVFS running as a daemon that:
  1. Mounts a FUSE filesystem presenting remote data as local files
  2. Runs an HTTP API for manifest queries, cache stats, and SSE events
  3. Watches for new files arriving from streaming adapters
  4. Provides an HTTP file proxy fallback for non-FUSE environments`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Apply CLI flag overrides to config
			if cfg.Serve == nil {
				cfg.Serve = &config.ServeConfig{}
			}
			if port > 0 {
				cfg.Serve.Port = port
			}
			if fuseEnabled {
				if cfg.Serve.FUSE == nil {
					cfg.Serve.FUSE = &config.FUSEConfig{}
				}
				if mountPoint != "" {
					cfg.Serve.FUSE.MountPoint = mountPoint
				}
				if cacheDir != "" {
					cfg.Serve.FUSE.CacheDir = cacheDir
				}
			}

			log.Info("starting serve mode",
				zap.String("name", cfg.Metadata.Name),
				zap.Bool("fuse", fuseEnabled),
				zap.Int("port", cfg.Serve.Port),
			)

			engine := serve.NewEngine(cfg, buildAdapterFactory(cmd.Context()), log)
			return engine.Run(cmd.Context())
		},
	}

	cmd.Flags().BoolVar(&fuseEnabled, "fuse", false, "enable FUSE virtual filesystem mount")
	cmd.Flags().IntVar(&port, "port", 0, "HTTP API port (default: 8090)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "directory for FUSE block cache")
	cmd.Flags().StringVar(&mountPoint, "mount-point", "", "FUSE mount point directory")

	return cmd
}
