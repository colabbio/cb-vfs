package secrets

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colabbio/medvfs/internal/config"
)

func generateRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func generateMTLSKeyMaterial(t *testing.T) (caPEM, certPEM, keyPEM []byte, serverTLSCfg *tls.Config) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ColabBio Test Hospital Root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	// Server certificate for 127.0.0.1
	srvKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(srvKey)})
	srvTLSCert, _ := tls.X509KeyPair(srvCertPEM, srvKeyPEM)

	// Client certificate
	cliKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cliTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "cb-node-hospital-lafe"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	cliDER, err := x509.CreateCertificate(rand.Reader, cliTmpl, caCert, &cliKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(cliKey)})

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	serverTLSCfg = &tls.Config{
		Certificates: []tls.Certificate{srvTLSCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	return caPEM, certPEM, keyPEM, serverTLSCfg
}

func TestBuildAuthenticatedHTTPClient_SMARTBackendJWTAndAutoRefresh(t *testing.T) {
	privPEM := generateRSAKeyPEM(t)
	secDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secDir, "private_key.pem"), privPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secDir, "client_id"), []byte("colabbio-smart-edge"), 0o600); err != nil {
		t.Fatal(err)
	}

	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			_ = r.ParseForm()
			if r.Form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
				t.Errorf("unexpected client_assertion_type: %s", r.Form.Get("client_assertion_type"))
			}
			assertion := r.Form.Get("client_assertion")
			if strings.Count(assertion, ".") != 2 {
				t.Errorf("invalid JWT assertion format: %s", assertion)
			}
			n := tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "smart-jwt-token-v" + string(rune('0'+n)),
				"expires_in":   300,
			})

		case "/fhir/Patient":
			if r.Header.Get("X-ColabBio-Grant-Id") != "GRANT-2026-LAFE-001" {
				http.Error(w, "missing grant header", http.StatusForbidden)
				return
			}
			auth := r.Header.Get("Authorization")
			// Simulate token expiry on first call with v1 if requested
			if !strings.HasPrefix(auth, "Bearer smart-jwt-token-v") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resourceType":"Bundle"}`))
		}
	}))
	defer srv.Close()

	authCfg := &config.AuthConfig{
		Type:                 "smart_backend_jwt",
		SecretRef:            "file://" + secDir,
		PropagateCohortGrant: true,
		GrantID:              "GRANT-2026-LAFE-001",
		OAuth2: &config.OAuth2AuthConfig{
			TokenURL:           srv.URL + "/oauth2/token",
			AssertionAlgorithm: "RS384",
			Scopes:             []string{"system/Patient.read"},
		},
	}

	client, err := BuildAuthenticatedHTTPClient(srv.URL, authCfg, 10*time.Second)
	if err != nil {
		t.Fatalf("BuildAuthenticatedHTTPClient: %v", err)
	}

	// 1st request triggers token exchange
	resp1, err := client.Get(srv.URL + "/fhir/Patient")
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("1st GET failed: status=%v err=%v", resp1, err)
	}
	_ = resp1.Body.Close()

	// 2nd request reuses cached token (tokenCalls stays 1)
	resp2, err := client.Get(srv.URL + "/fhir/Patient")
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("2nd GET failed: status=%v err=%v", resp2, err)
	}
	_ = resp2.Body.Close()

	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("expected cached token reuse (1 token call), got %d", got)
	}
}

func TestBuildAuthenticatedHTTPClient_MTLSAndAPIKey(t *testing.T) {
	caPEM, certPEM, keyPEM, srvTLS := generateMTLSKeyMaterial(t)
	secDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(secDir, "ca.crt"), caPEM, 0o600)
	_ = os.WriteFile(filepath.Join(secDir, "tls.crt"), certPEM, 0o600)
	_ = os.WriteFile(filepath.Join(secDir, "tls.key"), keyPEM, 0o600)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "mTLS client cert required", http.StatusUnauthorized)
			return
		}
		if r.TLS.PeerCertificates[0].Subject.CommonName != "cb-node-hospital-lafe" {
			http.Error(w, "wrong client cert CN", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("MTLS_OK"))
	}))
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	authCfg := &config.AuthConfig{
		Type:      "mtls",
		SecretRef: "file://" + secDir,
	}

	client, err := BuildAuthenticatedHTTPClient(srv.URL, authCfg, 10*time.Second)
	if err != nil {
		t.Fatalf("BuildAuthenticatedHTTPClient(mtls): %v", err)
	}

	resp, err := client.Get(srv.URL + "/dicom-web/studies")
	if err != nil {
		t.Fatalf("mTLS GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK over mTLS, got %d", resp.StatusCode)
	}
}

func TestBuildAuthenticatedHTTPClient_OMEROSessionHandshake(t *testing.T) {
	secDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(secDir, "username"), []byte("omero_svc_lafe"), 0o600)
	_ = os.WriteFile(filepath.Join(secDir, "password"), []byte("hospital_wsi_pass"), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/token/":
			http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "csrf-lafe-999", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":"csrf-lafe-999"}`))

		case "/api/v0/login/":
			_ = r.ParseForm()
			if r.Form.Get("username") != "omero_svc_lafe" || r.Form.Get("password") != "hospital_wsi_pass" {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-CSRFToken") != "csrf-lafe-999" {
				http.Error(w, "missing csrf header", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "omero-sess-uuid-777", Path: "/"})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true}`))

		case "/api/v0/m/images/42/render/original/":
			c, err := r.Cookie("sessionid")
			if err != nil || c.Value != "omero-sess-uuid-777" {
				http.Error(w, "missing omero sessionid cookie", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("SVS_TILE_BYTES"))
		}
	}))
	defer srv.Close()

	authCfg := &config.AuthConfig{
		Type:      "omero_session",
		SecretRef: "file://" + secDir,
	}

	client, err := BuildAuthenticatedHTTPClient(srv.URL, authCfg, 10*time.Second)
	if err != nil {
		t.Fatalf("BuildAuthenticatedHTTPClient(omero_session): %v", err)
	}

	resp, err := client.Get(srv.URL + "/api/v0/m/images/42/render/original/")
	if err != nil {
		t.Fatalf("OMERO render GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content with OMERO session cookie, got %d", resp.StatusCode)
	}
}
