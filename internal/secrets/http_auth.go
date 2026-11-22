package secrets

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/colabbio/medvfs/internal/config"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
)

// ResolveAuthBundle resolves a SecretBundle combining any SecretRef with inline
// fallback fields defined in config.AuthConfig.
func ResolveAuthBundle(ctx any, auth *config.AuthConfig) (SecretBundle, error) {
	bundle := make(SecretBundle)
	if auth == nil {
		return bundle, nil
	}
	if auth.SecretRef != "" {
		resolved, err := ResolveBundle(nil, auth.SecretRef)
		if err != nil {
			return nil, err
		}
		for k, v := range resolved {
			bundle[k] = v
		}
	}
	// Inline config overrides / dev fallbacks
	if auth.Token != "" {
		bundle["token"] = []byte(auth.Token)
		if _, ok := bundle["value"]; !ok {
			bundle["value"] = []byte(auth.Token)
		}
		if parts := strings.SplitN(auth.Token, ":", 2); len(parts) == 2 && !strings.Contains(auth.Token, "://") {
			if _, ok := bundle["username"]; !ok {
				bundle["username"] = []byte(parts[0])
				bundle["password"] = []byte(parts[1])
			}
		}
	}
	if auth.Username != "" {
		bundle["username"] = []byte(auth.Username)
	}
	if auth.Password != "" {
		bundle["password"] = []byte(auth.Password)
	}
	if auth.ClientID != "" {
		bundle["client_id"] = []byte(auth.ClientID)
	}
	if auth.OAuth2 != nil && auth.OAuth2.ClientID != "" {
		bundle["client_id"] = []byte(auth.OAuth2.ClientID)
	}
	if auth.ClientSecret != "" {
		bundle["client_secret"] = []byte(auth.ClientSecret)
	}
	return bundle, nil
}

// ResolveBasicCredentials extracts (username, password) from AuthConfig + SecretBundle.
func ResolveBasicCredentials(auth *config.AuthConfig) (username, password string, err error) {
	bundle, err := ResolveAuthBundle(nil, auth)
	if err != nil {
		return "", "", err
	}
	user := bundle.FirstString("username", "user", "omero_user", "rw_username", "ro_username")
	pass := bundle.FirstString("password", "pass", "omero_password", "rw_password", "ro_password")
	if user != "" && pass != "" {
		return user, pass, nil
	}
	// Check colon-delimited "user:password" in value or token
	raw := bundle.FirstString("value", "token")
	if parts := strings.SplitN(raw, ":", 2); len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return "", "", fmt.Errorf("basic/omero credentials require 'username' and 'password' (or 'user:pass') in secret_ref %q", auth.SecretRef)
}

// BuildTLSConfig builds a *tls.Config for mTLS or custom CA trust from a SecretBundle.
func BuildTLSConfig(auth *config.AuthConfig, bundle SecretBundle) (*tls.Config, error) {
	if auth == nil {
		return nil, nil
	}
	needsMTLS := auth.Type == "mtls" || auth.Type == "mtls_oauth2" || auth.MTLS != nil
	if !needsMTLS {
		return nil, nil
	}

	certKey := "tls.crt"
	keyKey := "tls.key"
	caKey := "ca.crt"
	var serverName string
	var insecureSkip bool

	if auth.MTLS != nil {
		if auth.MTLS.ClientCertKey != "" {
			certKey = auth.MTLS.ClientCertKey
		}
		if auth.MTLS.ClientKeyKey != "" {
			keyKey = auth.MTLS.ClientKeyKey
		}
		if auth.MTLS.CACertKey != "" {
			caKey = auth.MTLS.CACertKey
		}
		serverName = auth.MTLS.ServerNameOverride
		insecureSkip = auth.MTLS.InsecureSkipVerify
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: insecureSkip,
	}

	// Load custom Root CA if present
	if caPEM := bundle.Bytes(caKey); len(caPEM) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed to parse CA certificate from key %q", caKey)
		}
		tlsCfg.RootCAs = pool
	}

	// Load client certificate + private key if present
	certPEM := bundle.Bytes(certKey)
	keyPEM := bundle.Bytes(keyKey)
	if len(certPEM) > 0 || len(keyPEM) > 0 {
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			return nil, fmt.Errorf("mTLS requires both %q and %q in secret bundle", certKey, keyKey)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("loading mTLS X509KeyPair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	} else if auth.Type == "mtls" || auth.Type == "mtls_oauth2" {
		return nil, fmt.Errorf("auth.type %q requires client certificate (%s) and key (%s) in secret_ref %q",
			auth.Type, certKey, keyKey, auth.SecretRef)
	}

	return tlsCfg, nil
}

// BuildAuthenticatedHTTPClient builds an *http.Client pre-configured with mTLS,
// CookieJar (for OMERO sessions), OAuth2/SMART token auto-refresh, Basic, Bearer,
// API Key headers, and optional X-ColabBio-Grant-Id propagation (CB-SPEC-CRED-022 §5.2).
func BuildAuthenticatedHTTPClient(endpoint string, auth *config.AuthConfig, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})

	if auth == nil || auth.Type == "" || auth.Type == "none" {
		return &http.Client{
			Timeout: timeout,
			Jar:     jar,
		}, nil
	}

	bundle, err := ResolveAuthBundle(nil, auth)
	if err != nil {
		return nil, fmt.Errorf("resolve auth bundle: %w", err)
	}

	tlsCfg, err := BuildTLSConfig(auth, bundle)
	if err != nil {
		return nil, fmt.Errorf("configure mTLS: %w", err)
	}

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsCfg != nil {
		baseTransport.TLSClientConfig = tlsCfg
	}

	rt := &AuthenticatedRoundTripper{
		Base:     baseTransport,
		Endpoint: strings.TrimRight(endpoint, "/"),
		Auth:     auth,
		Bundle:   bundle,
		Jar:      jar,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: rt,
		Jar:       jar,
	}, nil
}

// AuthenticatedRoundTripper implements http.RoundTripper with automatic credential
// injection, OAuth2 / SMART private_key_jwt token caching & refresh, and OMERO session
// CSRF/cookie management.
type AuthenticatedRoundTripper struct {
	Base     http.RoundTripper
	Endpoint string
	Auth     *config.AuthConfig
	Bundle   SecretBundle
	Jar      http.CookieJar

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
	omeroCSRF   string
	omeroLogged bool
}

// RoundTrip executes a single HTTP transaction, injecting upstream authentication headers.
func (rt *AuthenticatedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := rt.Base
	if base == nil {
		base = http.DefaultTransport
	}

	// Clone request before mutating headers (per http.RoundTripper contract)
	cloned := req.Clone(req.Context())

	// Propagate X-ColabBio-Grant-Id if configured
	if rt.Auth != nil && rt.Auth.PropagateCohortGrant {
		grantID := rt.Auth.GrantID
		if grantID == "" {
			grantID = os.Getenv("COLABBIO_GRANT_ID")
		}
		if grantID != "" && cloned.Header.Get("X-ColabBio-Grant-Id") == "" {
			cloned.Header.Set("X-ColabBio-Grant-Id", grantID)
		}
	}

	if err := rt.applyAuth(cloned); err != nil {
		return nil, err
	}

	resp, err := base.RoundTrip(cloned)
	if err != nil {
		return nil, err
	}

	// Transparent retry on 401 Unauthorized for OAuth2/SMART or OMERO session expiration
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && rt.supportsAutoRefresh() && req.Body == nil {
		rt.invalidateCachedSession()
		retryReq := req.Clone(req.Context())
		if err := rt.applyAuth(retryReq); err == nil {
			_ = resp.Body.Close()
			return base.RoundTrip(retryReq)
		}
	}

	return resp, nil
}

func (rt *AuthenticatedRoundTripper) supportsAutoRefresh() bool {
	if rt.Auth == nil {
		return false
	}
	switch rt.Auth.Type {
	case "oauth2_client_credentials", "smart_on_fhir", "smart_backend_jwt", "mtls_oauth2", "omero_session":
		return true
	default:
		return false
	}
}

func (rt *AuthenticatedRoundTripper) invalidateCachedSession() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.cachedToken = ""
	rt.tokenExpiry = time.Time{}
	rt.omeroLogged = false
}

func (rt *AuthenticatedRoundTripper) applyAuth(req *http.Request) error {
	if rt.Auth == nil {
		return nil
	}
	switch rt.Auth.Type {
	case "", "none", "mtls":
		// mTLS is handled at the transport TLS layer
		return nil

	case "bearer":
		tok := rt.Bundle.FirstString("token", "access_token", "value")
		if tok == "" {
			return fmt.Errorf("bearer auth: missing token in config or secret_ref %q", rt.Auth.SecretRef)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil

	case "basic":
		user := rt.Bundle.FirstString("username", "user")
		pass := rt.Bundle.FirstString("password", "pass")
		if user == "" || pass == "" {
			raw := rt.Bundle.FirstString("value", "token")
			if parts := strings.SplitN(raw, ":", 2); len(parts) == 2 {
				user, pass = parts[0], parts[1]
			}
		}
		if user == "" {
			return fmt.Errorf("basic auth: missing username/password in config or secret_ref %q", rt.Auth.SecretRef)
		}
		req.SetBasicAuth(user, pass)
		return nil

	case "api_key":
		keyVal := rt.Bundle.FirstString("api_key", "token", "value")
		if keyVal == "" {
			return fmt.Errorf("api_key auth: missing api_key in secret_ref %q", rt.Auth.SecretRef)
		}
		headerName := "X-API-Key"
		prefix := ""
		if rt.Auth.APIKey != nil {
			if rt.Auth.APIKey.HeaderName != "" {
				headerName = rt.Auth.APIKey.HeaderName
			}
			prefix = rt.Auth.APIKey.Prefix
		}
		req.Header.Set(headerName, prefix+keyVal)
		return nil

	case "oauth2_client_credentials", "smart_on_fhir", "smart_backend_jwt", "mtls_oauth2":
		// For mtls_oauth2 without an OAuth2 token URL, allow fallback to static bearer token if provided
		if rt.Auth.Type == "mtls_oauth2" && rt.resolveTokenURL() == "" {
			if tok := rt.Bundle.FirstString("token", "access_token"); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			return nil
		}
		tok, err := rt.getOrRefreshOAuth2Token(req)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil

	case "omero_session":
		return rt.ensureOMEROSession(req)

	default:
		return fmt.Errorf("unsupported auth.type %q", rt.Auth.Type)
	}
}

func (rt *AuthenticatedRoundTripper) resolveTokenURL() string {
	if rt.Auth.OAuth2 != nil && rt.Auth.OAuth2.TokenURL != "" {
		return rt.Auth.OAuth2.TokenURL
	}
	if rt.Auth.TokenURL != "" {
		return rt.Auth.TokenURL
	}
	return rt.Bundle.FirstString("token_url")
}

func (rt *AuthenticatedRoundTripper) getOrRefreshOAuth2Token(req *http.Request) (string, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	skew := 60 * time.Second
	if rt.Auth.OAuth2 != nil && rt.Auth.OAuth2.RefreshSkewSeconds > 0 {
		skew = time.Duration(rt.Auth.OAuth2.RefreshSkewSeconds) * time.Second
	}

	if rt.cachedToken != "" && (rt.tokenExpiry.IsZero() || time.Now().Add(skew).Before(rt.tokenExpiry)) {
		return rt.cachedToken, nil
	}

	tokenURL := rt.resolveTokenURL()
	if tokenURL == "" {
		return "", fmt.Errorf("%s auth: token_url is required", rt.Auth.Type)
	}

	clientID := rt.Bundle.FirstString("client_id")
	if clientID == "" {
		return "", fmt.Errorf("%s auth: client_id is required", rt.Auth.Type)
	}

	scopes := rt.Auth.Scope
	if rt.Auth.OAuth2 != nil && len(rt.Auth.OAuth2.Scopes) > 0 {
		scopes = strings.Join(rt.Auth.OAuth2.Scopes, " ")
	}
	if scopes == "" {
		scopes = rt.Bundle.FirstString("scope", "scopes")
	}
	if scopes == "" {
		scopes = "system/*.read"
	}

	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {scopes},
	}

	// Determine whether to use RFC 7523 private_key_jwt assertion or standard client_secret
	privKeyPEM := rt.Bundle.Bytes("private_key.pem")
	if len(privKeyPEM) == 0 {
		privKeyPEM = rt.Bundle.Bytes("private_key")
	}

	if rt.Auth.Type == "smart_backend_jwt" || len(privKeyPEM) > 0 {
		if len(privKeyPEM) == 0 {
			return "", fmt.Errorf("smart_backend_jwt requires 'private_key.pem' in secret_ref %q", rt.Auth.SecretRef)
		}
		alg := "RS384"
		aud := tokenURL
		kid := rt.Bundle.FirstString("key_id", "kid")
		if rt.Auth.OAuth2 != nil {
			if rt.Auth.OAuth2.AssertionAlgorithm != "" {
				alg = rt.Auth.OAuth2.AssertionAlgorithm
			}
			if rt.Auth.OAuth2.Audience != "" {
				aud = rt.Auth.OAuth2.Audience
			}
			if rt.Auth.OAuth2.KeyID != "" {
				kid = rt.Auth.OAuth2.KeyID
			}
		}
		assertion, err := BuildSMARTClientAssertionJWT(clientID, aud, kid, alg, privKeyPEM)
		if err != nil {
			return "", fmt.Errorf("sign SMART private_key_jwt assertion: %w", err)
		}
		form.Set("client_id", clientID)
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion)
	} else {
		clientSecret := rt.Bundle.FirstString("client_secret", "secret", "value")
		if clientSecret == "" {
			return "", fmt.Errorf("%s auth: client_secret is required in secret_ref %q", rt.Auth.Type, rt.Auth.SecretRef)
		}
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}

	tokReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.Header.Set("Accept", "application/json")

	base := rt.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(tokReq)
	if err != nil {
		return "", fmt.Errorf("oauth2 token request to %s: %w", tokenURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("oauth2 token endpoint HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tokResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokResp); err != nil {
		return "", fmt.Errorf("decode oauth2 token response: %w", err)
	}
	if tokResp.AccessToken == "" {
		return "", fmt.Errorf("oauth2 token endpoint returned empty access_token")
	}

	rt.cachedToken = tokResp.AccessToken
	if tokResp.ExpiresIn > 0 {
		rt.tokenExpiry = time.Now().Add(time.Duration(tokResp.ExpiresIn) * time.Second)
	} else {
		rt.tokenExpiry = time.Now().Add(5 * time.Minute)
	}
	return rt.cachedToken, nil
}

// ensureOMEROSession negotiates the 2-step OMERO.web CSRF + sessionid cookie handshake
// and attaches cookies + X-CSRFToken to the outgoing request.
func (rt *AuthenticatedRoundTripper) ensureOMEROSession(req *http.Request) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	// Check if an explicit OMERO sessionid was already supplied in the SecretBundle
	if sessUUID := rt.Bundle.FirstString("omero_session_uuid", "sessionid", "omero_session"); sessUUID != "" && !rt.omeroLogged {
		if u, err := url.Parse(rt.Endpoint); err == nil && rt.Jar != nil {
			rt.Jar.SetCookies(u, []*http.Cookie{{Name: "sessionid", Value: sessUUID, Path: "/"}})
		}
		rt.omeroLogged = true
	}

	if !rt.omeroLogged && rt.Endpoint != "" {
		base := rt.Base
		if base == nil {
			base = http.DefaultTransport
		}

		// Step 1: GET /api/v0/token/
		tokenURL := rt.Endpoint + "/api/v0/token/"
		tReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, tokenURL, nil)
		if err != nil {
			return err
		}
		tResp, err := base.RoundTrip(tReq)
		if err != nil {
			return fmt.Errorf("omero token handshake: %w", err)
		}
		var tokData struct {
			Data string `json:"data"`
		}
		_ = json.NewDecoder(tResp.Body).Decode(&tokData)
		if rt.Jar != nil {
			if u, err := url.Parse(tokenURL); err == nil {
				rt.Jar.SetCookies(u, tResp.Cookies())
			}
		}
		_ = tResp.Body.Close()
		rt.omeroCSRF = tokData.Data

		// Step 2: POST /api/v0/login/
		user, pass, err := ResolveBasicCredentials(rt.Auth)
		if err != nil {
			return err
		}
		serverID := rt.Bundle.FirstString("server_id")
		if serverID == "" {
			serverID = "1"
		}
		loginURL := rt.Endpoint + "/api/v0/login/"
		form := url.Values{
			"username":  {user},
			"password":  {pass},
			"server":    {serverID},
			"csrftoken": {rt.omeroCSRF},
		}
		lReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, loginURL, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		lReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		lReq.Header.Set("X-CSRFToken", rt.omeroCSRF)
		lReq.Header.Set("Referer", rt.Endpoint+"/")
		if rt.Jar != nil {
			if u, err := url.Parse(loginURL); err == nil {
				for _, c := range rt.Jar.Cookies(u) {
					lReq.AddCookie(c)
				}
			}
		}
		lResp, err := base.RoundTrip(lReq)
		if err != nil {
			return fmt.Errorf("omero login handshake: %w", err)
		}
		if rt.Jar != nil {
			if u, err := url.Parse(loginURL); err == nil {
				rt.Jar.SetCookies(u, lResp.Cookies())
			}
		}
		body, _ := io.ReadAll(lResp.Body)
		_ = lResp.Body.Close()
		if lResp.StatusCode != http.StatusOK {
			return fmt.Errorf("omero login failed (%d): %s", lResp.StatusCode, string(body))
		}
		rt.omeroLogged = true
	}

	if rt.omeroCSRF != "" {
		req.Header.Set("X-CSRFToken", rt.omeroCSRF)
	}
	if rt.Jar != nil {
		for _, c := range rt.Jar.Cookies(req.URL) {
			req.AddCookie(c)
		}
	}
	return nil
}

// BuildSMARTClientAssertionJWT creates an RFC 7523 JWT client assertion signed with
// RS256, RS384, or ES384 for SMART-on-FHIR Backend Services authentication.
func BuildSMARTClientAssertionJWT(clientID, audience, keyID, alg string, privateKeyPEM []byte) (string, error) {
	if alg == "" {
		alg = "RS384"
	}
	header := map[string]string{
		"alg": alg,
		"typ": "JWT",
	}
	if keyID != "" {
		header["kid"] = keyID
	}

	now := time.Now().Unix()
	claims := map[string]any{
		"iss": clientID,
		"sub": clientID,
		"aud": audience,
		"jti": uuid.NewString(),
		"iat": now,
		"exp": now + 300, // 5 minutes per SMART Backend Services spec
	}

	hJSON, _ := json.Marshal(header)
	cJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hJSON) + "." + base64.RawURLEncoding.EncodeToString(cJSON)

	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return "", fmt.Errorf("invalid PEM block in private key")
	}

	var hashFunc crypto.Hash
	switch strings.ToUpper(alg) {
	case "RS256":
		hashFunc = crypto.SHA256
	case "RS384", "ES384":
		hashFunc = crypto.SHA384
	default:
		return "", fmt.Errorf("unsupported SMART JWT algorithm %q (supported: RS256, RS384, ES384)", alg)
	}

	var digest []byte
	if hashFunc == crypto.SHA256 {
		sum := sha256.Sum256([]byte(signingInput))
		digest = sum[:]
	} else {
		sum := sha512.Sum384([]byte(signingInput))
		digest = sum[:]
	}

	// Parse PKCS#8 or PKCS#1 / EC private key
	privKey, err := parsePrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}

	switch k := privKey.(type) {
	case *rsa.PrivateKey:
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, hashFunc, digest)
		if err != nil {
			return "", err
		}
		return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil

	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest)
		if err != nil {
			return "", err
		}
		keyBytes := (k.Curve.Params().BitSize + 7) / 8
		sig := make([]byte, 2*keyBytes)
		rBytes := r.Bytes()
		sBytes := s.Bytes()
		copy(sig[keyBytes-len(rBytes):keyBytes], rBytes)
		copy(sig[2*keyBytes-len(sBytes):], sBytes)
		return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil

	default:
		return "", fmt.Errorf("unsupported private key type %T", privKey)
	}
}

func parsePrivateKey(der []byte) (any, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("unable to parse private key as PKCS#8, PKCS#1, or EC")
}
