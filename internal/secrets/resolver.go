package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Resolve resolves a secret reference string to its plaintext value.
//
// Supported schemes:
//   - k8s://namespace/secret-name/key  — reads from K8s Secret API
//   - env://VAR_NAME                   — reads from environment variable
//   - file:///absolute/path            — reads from a local file
//   - (no scheme)                      — returned as-is (literal value, dev only)
func Resolve(ctx context.Context, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env://"):
		varName := strings.TrimPrefix(ref, "env://")
		val := os.Getenv(varName)
		if val == "" {
			return "", fmt.Errorf("env var %q is not set", varName)
		}
		return val, nil

	case strings.HasPrefix(ref, "file://"):
		path := strings.TrimPrefix(ref, "file://")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading secret file %q: %w", path, err)
		}
		return strings.TrimSpace(string(data)), nil

	case strings.HasPrefix(ref, "k8s://"):
		return resolveK8sSecret(ctx, ref)

	default:
		// Literal value — acceptable for local development
		return ref, nil
	}
}

// resolveK8sSecret reads a value from a Kubernetes Secret.
// Format: k8s://namespace/secret-name/key
func resolveK8sSecret(ctx context.Context, ref string) (string, error) {
	// k8s:// + namespace/secret/key
	parts := strings.SplitN(strings.TrimPrefix(ref, "k8s://"), "/", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid k8s secret ref %q: expected k8s://namespace/secret-name/key", ref)
	}
	namespace, secretName, key := parts[0], parts[1], parts[2]

	// Try mounted secret file first (standard K8s volume mount at /var/run/secrets/)
	mountPath := fmt.Sprintf("/var/run/secrets/colabbio/%s/%s/%s", namespace, secretName, key)
	if data, err := os.ReadFile(mountPath); err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	// Fallback: K8s API via in-cluster config
	// (requires k8s.io/client-go — added when k8s integration is needed)
	return "", fmt.Errorf("k8s secret %s/%s[%s]: not found via mount or API (k8s API client not yet initialised)", namespace, secretName, key)
}
