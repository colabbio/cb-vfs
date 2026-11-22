package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// K8sSecretMountRoot is the base directory where Kubernetes Secrets are mounted
// in ColabBio pods (configurable in tests).
var K8sSecretMountRoot = "/var/run/secrets/colabbio"

// VaultAgentMountRoot is the base directory where HashiCorp Vault / OpenBao Agent
// renders secrets on hospital edge nodes (configurable in tests).
var VaultAgentMountRoot = "/vault/secrets"

// SecretBundle holds a multikey map of resolved secret payloads as specified
// in CB-SPEC-CRED-022 (e.g., "username", "password", "token", "client_id",
// "client_secret", "private_key.pem", "tls.crt", "tls.key", "ca.crt").
type SecretBundle map[string][]byte

// String returns the trimmed string value for key, or empty string if missing.
func (b SecretBundle) String(key string) string {
	if b == nil {
		return ""
	}
	if v, ok := b[key]; ok {
		return strings.TrimSpace(string(v))
	}
	if v, ok := b[strings.ToLower(key)]; ok {
		return strings.TrimSpace(string(v))
	}
	return ""
}

// FirstString returns the first non-empty string value among the candidate keys.
func (b SecretBundle) FirstString(keys ...string) string {
	for _, k := range keys {
		if val := b.String(k); val != "" {
			return val
		}
	}
	return ""
}

// Bytes returns the raw byte slice for key (or nil if not found).
func (b SecretBundle) Bytes(key string) []byte {
	if b == nil {
		return nil
	}
	if v, ok := b[key]; ok {
		return v
	}
	if v, ok := b[strings.ToLower(key)]; ok {
		return v
	}
	return nil
}

// Resolve resolves a secret reference string to a single scalar plaintext value.
//
// Supported schemes:
//   - k8s://namespace/secret-name[/key] — reads from mounted K8s Secret volume
//   - vault://mount/path[#field]        — reads from Vault Agent file or env fallback
//   - env://VAR_NAME                    — reads from environment variable
//   - file:///absolute/path             — reads from a local file
//   - (no scheme)                       — returned as-is (literal value, dev only)
func Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty secret reference")
	}
	if !strings.Contains(ref, "://") {
		return ref, nil
	}
	bundle, err := ResolveBundle(ctx, ref)
	if err != nil {
		return "", err
	}
	if v := bundle.FirstString("value", "token", "password", "dsn", "api_key", "client_secret"); v != "" {
		return v, nil
	}
	// If there is exactly one key in the bundle, return its value
	if len(bundle) == 1 {
		for _, v := range bundle {
			return strings.TrimSpace(string(v)), nil
		}
	}
	return "", fmt.Errorf("secret reference %q resolved to %d keys without an explicit target key", ref, len(bundle))
}

// ResolveBundle resolves a secret_ref URI into a multikey SecretBundle (CB-SPEC-CRED-022 §3.1 & §5.1).
//
// Supported schemes:
//   - k8s://namespace/secret-name[/key]
//   - vault://mount/path[#field]
//   - env://PREFIX_OR_VAR
//   - file:///path/to/dir_or_json_or_file
func ResolveBundle(ctx context.Context, ref string) (SecretBundle, error) {
	_ = ctx
	if ref == "" {
		return SecretBundle{}, nil
	}

	switch {
	case strings.HasPrefix(ref, "env://"):
		return resolveEnvBundle(strings.TrimPrefix(ref, "env://"))

	case strings.HasPrefix(ref, "file://"):
		return resolveFileBundle(strings.TrimPrefix(ref, "file://"))

	case strings.HasPrefix(ref, "k8s://"):
		return resolveK8sBundle(strings.TrimPrefix(ref, "k8s://"))

	case strings.HasPrefix(ref, "vault://"):
		return resolveVaultBundle(strings.TrimPrefix(ref, "vault://"))

	default:
		// Literal value (dev only)
		return SecretBundle{
			"value": []byte(ref),
			"token": []byte(ref),
		}, nil
	}
}

// resolveEnvBundle resolves either a direct environment variable or all variables
// sharing the given prefix (e.g., env://LAFE_FHIR reads LAFE_FHIR_CLIENT_ID -> "client_id").
func resolveEnvBundle(target string) (SecretBundle, error) {
	if target == "" {
		return nil, fmt.Errorf("env:// reference missing variable or prefix name")
	}
	bundle := make(SecretBundle)

	// 1. Direct variable match
	if directVal, ok := os.LookupEnv(target); ok && directVal != "" {
		trimmed := strings.TrimSpace(directVal)
		// If the env var holds a JSON object, parse its keys into the bundle
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			_ = parseJSONIntoBundle([]byte(trimmed), bundle)
		}
		bundle["value"] = []byte(trimmed)
		bundle[strings.ToLower(target)] = []byte(trimmed)
		// Support COLON-separated basic credentials (user:pass) in a single env var
		if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 && !strings.Contains(trimmed, "://") {
			if _, hasUser := bundle["username"]; !hasUser {
				bundle["username"] = []byte(parts[0])
				bundle["password"] = []byte(parts[1])
			}
		}
	}

	// 2. Prefix scan: <TARGET>_<KEY>=<VAL>
	prefix := target + "_"
	for _, kv := range os.Environ() {
		k, v, found := strings.Cut(kv, "=")
		if !found || !strings.HasPrefix(k, prefix) || v == "" {
			continue
		}
		subKey := strings.ToLower(strings.TrimPrefix(k, prefix))
		bundle[subKey] = []byte(v)
		// Map common dot-filename conventions: TLS_CRT -> tls.crt, PRIVATE_KEY_PEM -> private_key.pem
		switch subKey {
		case "tls_crt":
			bundle["tls.crt"] = []byte(v)
		case "tls_key":
			bundle["tls.key"] = []byte(v)
		case "ca_crt":
			bundle["ca.crt"] = []byte(v)
		case "private_key_pem":
			bundle["private_key.pem"] = []byte(v)
		case "service_account_json":
			bundle["service_account.json"] = []byte(v)
		}
	}

	if len(bundle) == 0 {
		return nil, fmt.Errorf("env var or prefix %q is not set", target)
	}
	return bundle, nil
}

// resolveFileBundle reads a secret directory, a JSON file, a key=value file, or a scalar file.
func resolveFileBundle(path string) (SecretBundle, error) {
	if path == "" {
		return nil, fmt.Errorf("file:// reference is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading secret path %q: %w", path, err)
	}

	if info.IsDir() {
		return readDirectoryBundle(path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading secret file %q: %w", path, err)
	}

	bundle := make(SecretBundle)
	trimmed := strings.TrimSpace(string(data))
	bundle["value"] = []byte(trimmed)
	bundle[filepath.Base(path)] = data

	// Check if file is JSON object
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		_ = parseJSONIntoBundle(data, bundle)
		return bundle, nil
	}

	// Check if file contains basic user:pass
	if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 && !strings.Contains(trimmed, "\n") && !strings.Contains(trimmed, "://") {
		bundle["username"] = []byte(parts[0])
		bundle["password"] = []byte(parts[1])
	}

	return bundle, nil
}

// resolveK8sBundle reads a Kubernetes Secret from the mounted secret directory:
//   - k8s://namespace/secret-name       -> reads all files in /var/run/secrets/colabbio/namespace/secret-name/
//   - k8s://secret-name                 -> reads all files in /var/run/secrets/colabbio/secret-name/ (or default ns)
//   - k8s://namespace/secret-name/key   -> reads specific key file and populates bundle[key] + bundle["value"]
func resolveK8sBundle(raw string) (SecretBundle, error) {
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid k8s secret ref %q: expected k8s://namespace/secret-name[/key]", "k8s://"+raw)
	}

	var candidatePaths []string
	var explicitKey string

	if len(parts) == 2 {
		// Could be k8s://namespace/secret-name OR k8s://secret-name/key
		nsOrSecret, secretOrKey := parts[0], parts[1]
		candidatePaths = append(candidatePaths,
			filepath.Join(K8sSecretMountRoot, nsOrSecret, secretOrKey),
			filepath.Join(K8sSecretMountRoot, nsOrSecret),
		)
		explicitKey = secretOrKey
	} else {
		namespace, secretName := parts[0], parts[1]
		explicitKey = strings.Join(parts[2:], "/")
		candidatePaths = append(candidatePaths,
			filepath.Join(K8sSecretMountRoot, namespace, secretName, explicitKey),
			filepath.Join(K8sSecretMountRoot, namespace, secretName),
			filepath.Join(K8sSecretMountRoot, secretName, explicitKey),
			filepath.Join(K8sSecretMountRoot, secretName),
		)
	}

	for _, p := range candidatePaths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			bundle, err := readDirectoryBundle(p)
			if err != nil {
				return nil, err
			}
			if explicitKey != "" {
				if val, ok := bundle[explicitKey]; ok {
					bundle["value"] = val
				}
			}
			return bundle, nil
		}
		// Single mounted key file
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(string(data))
		keyName := filepath.Base(p)
		bundle := SecretBundle{
			keyName: []byte(trimmed),
			"value": []byte(trimmed),
		}
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			_ = parseJSONIntoBundle(data, bundle)
		}
		return bundle, nil
	}

	// Environment fallback for local/CI runs where K8s secrets are injected via env vars
	// e.g. k8s://colabbio-secrets/fhir-token -> COLABBIO_SECRET_FHIR_TOKEN
	secretSlug := strings.ToUpper(strings.ReplaceAll(parts[len(parts)-1], "-", "_"))
	if envBundle, err := resolveEnvBundle("COLABBIO_SECRET_" + secretSlug); err == nil {
		return envBundle, nil
	}

	return nil, fmt.Errorf("k8s secret %q: not found under %s or COLABBIO_SECRET_%s env fallback", "k8s://"+raw, K8sSecretMountRoot, secretSlug)
}

// resolveVaultBundle reads a secret rendered by HashiCorp Vault / OpenBao Agent
// under /vault/secrets/<mount>/<path> (or fragment #field).
func resolveVaultBundle(raw string) (SecretBundle, error) {
	pathPart, field, _ := strings.Cut(raw, "#")
	pathPart = strings.Trim(pathPart, "/")
	if pathPart == "" {
		return nil, fmt.Errorf("invalid vault secret ref %q", "vault://"+raw)
	}

	candidates := []string{
		filepath.Join(VaultAgentMountRoot, pathPart),
		filepath.Join(VaultAgentMountRoot, pathPart+".json"),
		filepath.Join(VaultAgentMountRoot, filepath.Base(pathPart)),
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			bundle, err := resolveFileBundle(p)
			if err != nil {
				return nil, err
			}
			if field != "" {
				if val := bundle.Bytes(field); val != nil {
					bundle["value"] = val
				} else {
					return nil, fmt.Errorf("vault secret %q does not contain field %q", "vault://"+raw, field)
				}
			}
			return bundle, nil
		}
	}

	return nil, fmt.Errorf("vault secret %q not found under %s", "vault://"+raw, VaultAgentMountRoot)
}

func readDirectoryBundle(dir string) (SecretBundle, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading secret directory %q: %w", dir, err)
	}
	bundle := make(SecretBundle)
	for _, entry := range entries {
		name := entry.Name()
		// Skip Kubernetes atomic symlink directories (..data, ..2026_11_...)
		if strings.HasPrefix(name, ".") || entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		bundle[name] = data
		trimmed := strings.TrimSpace(string(data))
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			_ = parseJSONIntoBundle(data, bundle)
		}
	}
	if len(bundle) == 0 {
		return nil, fmt.Errorf("secret directory %q contains no secret files", dir)
	}
	return bundle, nil
}

func parseJSONIntoBundle(data []byte, bundle SecretBundle) error {
	var rawMap map[string]any
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return err
	}
	// Unwrap Vault KV v2 {"data": {"data": {...}}} if present
	if inner, ok := rawMap["data"].(map[string]any); ok {
		if kv2, ok2 := inner["data"].(map[string]any); ok2 {
			rawMap = kv2
		} else {
			rawMap = inner
		}
	}
	for k, v := range rawMap {
		switch typed := v.(type) {
		case string:
			bundle[k] = []byte(typed)
		default:
			enc, err := json.Marshal(typed)
			if err == nil {
				bundle[k] = enc
			}
		}
	}
	return nil
}
