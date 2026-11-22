package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBundle_EnvDirectAndPrefix(t *testing.T) {
	t.Setenv("TEST_CRED_CLIENT_ID", "colabbio-edge-01")
	t.Setenv("TEST_CRED_CLIENT_SECRET", "super-secret-xyz")
	t.Setenv("TEST_CRED_TLS_CRT", "-----BEGIN CERTIFICATE-----")

	bundle, err := ResolveBundle(context.Background(), "env://TEST_CRED")
	if err != nil {
		t.Fatalf("ResolveBundle(env://TEST_CRED): %v", err)
	}

	if got := bundle.String("client_id"); got != "colabbio-edge-01" {
		t.Errorf("client_id = %q, want colabbio-edge-01", got)
	}
	if got := bundle.String("client_secret"); got != "super-secret-xyz" {
		t.Errorf("client_secret = %q, want super-secret-xyz", got)
	}
	if got := bundle.String("tls.crt"); got != "-----BEGIN CERTIFICATE-----" {
		t.Errorf("tls.crt = %q, want cert header", got)
	}
}

func TestResolveBundle_K8sMountedDirectoryAndSingleKey(t *testing.T) {
	tmpRoot := t.TempDir()
	origRoot := K8sSecretMountRoot
	K8sSecretMountRoot = tmpRoot
	defer func() { K8sSecretMountRoot = origRoot }()

	secretDir := filepath.Join(tmpRoot, "colabbio-edge", "lafe-omero-secret")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "username"), []byte("patologo_lafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "password"), []byte("omero_hospital_pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Resolve full directory bundle
	bundle, err := ResolveBundle(context.Background(), "k8s://colabbio-edge/lafe-omero-secret")
	if err != nil {
		t.Fatalf("ResolveBundle(k8s dir): %v", err)
	}
	if bundle.String("username") != "patologo_lafe" || bundle.String("password") != "omero_hospital_pass" {
		t.Errorf("unexpected bundle contents: user=%q pass=%q", bundle.String("username"), bundle.String("password"))
	}

	// 2. Resolve single scalar key via Resolve()
	pass, err := Resolve(context.Background(), "k8s://colabbio-edge/lafe-omero-secret/password")
	if err != nil {
		t.Fatalf("Resolve(k8s key): %v", err)
	}
	if pass != "omero_hospital_pass" {
		t.Errorf("got %q, want omero_hospital_pass", pass)
	}
}

func TestResolveBundle_VaultKV2AndFileJSON(t *testing.T) {
	tmpVault := t.TempDir()
	origVault := VaultAgentMountRoot
	VaultAgentMountRoot = tmpVault
	defer func() { VaultAgentMountRoot = origVault }()

	vaultFile := filepath.Join(tmpVault, "hospital", "fhir-smart.json")
	if err := os.MkdirAll(filepath.Dir(vaultFile), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `{"data":{"data":{"client_id":"ucsf-smart-app","token":"vault-jwt-123"}}}`
	if err := os.WriteFile(vaultFile, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	bundle, err := ResolveBundle(context.Background(), "vault://hospital/fhir-smart#client_id")
	if err != nil {
		t.Fatalf("ResolveBundle(vault): %v", err)
	}
	if bundle.String("client_id") != "ucsf-smart-app" {
		t.Errorf("client_id = %q, want ucsf-smart-app", bundle.String("client_id"))
	}
	if bundle.String("value") != "ucsf-smart-app" {
		t.Errorf("value (#client_id) = %q, want ucsf-smart-app", bundle.String("value"))
	}
}
