package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthPrintsAuthorizationURL(t *testing.T) {
	var output strings.Builder
	auth := newAuth("credentials", "host", &output)
	const address = "https://auth.openai.com/example"
	if err := auth.presentURL(address); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "a-chatgpt-proxy: open this URL in a browser:\n"+address+"\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestAuthLogin(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	key := testRSAKey(t)
	const keyID = "test-key"

	var server *httptest.Server
	var mu sync.Mutex
	var authorizeQuery url.Values
	var tokenForm url.Values
	var nonce string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery":
			writeAuthJSON(t, w, testProvider(server.URL))
		case "/jwks":
			writeAuthJSON(t, w, testJWKS(&key.PublicKey, keyID))
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			tokenForm = cloneValues(r.PostForm)
			requestNonce := nonce
			mu.Unlock()
			idToken := signTestJWT(t, key, keyID, map[string]any{
				"iss": openAIIssuer, "sub": "subject-1", "aud": "oaiapp_issued",
				"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": requestNonce,
				"email": "user@example.com",
			})
			writeAuthJSON(t, w, tokenResponse{
				AccessToken: "access-1", RefreshToken: "refresh-1", IDToken: idToken,
				TokenType: "Bearer", ExpiresIn: 3600, Scope: requestedScopes,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	directory := t.TempDir()
	credentialPath := filepath.Join(directory, "credentials")
	hostPath := filepath.Join(directory, "host")
	auth := newAuth(credentialPath, hostPath, io.Discard)
	auth.discoveryURL = server.URL + "/discovery"
	auth.now = func() time.Time { return now }
	auth.presentURL = func(address string) error {
		authorizationURL, err := url.Parse(address)
		if err != nil {
			return err
		}
		query := authorizationURL.Query()
		mu.Lock()
		authorizeQuery = cloneValues(query)
		nonce = query.Get("nonce")
		mu.Unlock()
		callback, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			return err
		}
		callback.RawQuery = url.Values{
			"code":      {"authorization-code"},
			"state":     {query.Get("state")},
			"client_id": {"oaiapp_issued"},
		}.Encode()
		response, err := http.Get(callback.String())
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		return err
	}

	email, err := auth.login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@example.com" {
		t.Fatalf("email = %q", email)
	}
	mu.Lock()
	gotAuthorize := authorizeQuery
	gotTokenForm := tokenForm
	mu.Unlock()
	if gotAuthorize.Get("client_id") != dynamicClientID || gotAuthorize.Get("agent_name_hint") != "a-chatgpt-proxy" {
		t.Fatalf("authorization query = %v", gotAuthorize)
	}
	if gotAuthorize.Get("scope") != requestedScopes || gotAuthorize.Get("resource") != openAIResource || gotAuthorize.Get("ext_agent_host_id") == "" {
		t.Fatalf("authorization query = %v", gotAuthorize)
	}
	if gotTokenForm.Get("client_id") != "oaiapp_issued" || gotTokenForm.Get("grant_type") != "authorization_code" || gotTokenForm.Get("resource") != openAIResource {
		t.Fatalf("token form = %v", gotTokenForm)
	}
	if gotTokenForm.Get("redirect_uri") != gotAuthorize.Get("redirect_uri") || gotTokenForm.Get("code_verifier") == "" {
		t.Fatalf("token form = %v", gotTokenForm)
	}

	cred, err := readCredentials(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if cred.ClientID != "oaiapp_issued" || cred.Subject != "subject-1" || cred.RefreshToken != "refresh-1" {
		t.Fatalf("credentials = %#v", cred)
	}
	info, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential mode = %o", info.Mode().Perm())
	}
	hostData, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	if !validHostID(strings.TrimSpace(string(hostData))) {
		t.Fatalf("host ID = %q", hostData)
	}
	if token, err := auth.token(context.Background()); err != nil || token != "access-1" {
		t.Fatalf("token = %q, error = %v", token, err)
	}
}

func TestAuthRefreshesRotatingTokenOnce(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	credentialPath := filepath.Join(directory, "credentials")
	old := credentials{
		Email: "user@example.com", Issuer: openAIIssuer, Subject: "subject-1", ClientID: "oaiapp_issued",
		AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresIn: 3600, Scopes: strings.Fields(requestedScopes), SavedAt: now.Add(-3590 * time.Second),
	}
	if err := writeCredentials(credentialPath, old); err != nil {
		t.Fatal(err)
	}

	var refreshes atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery":
			writeAuthJSON(t, w, testProvider(server.URL))
		case "/token":
			refreshes.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "old-refresh" || r.PostForm.Get("client_id") != old.ClientID || r.PostForm.Has("scope") {
				t.Errorf("refresh form = %v", r.PostForm)
			}
			writeAuthJSON(t, w, tokenResponse{
				AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer",
				ExpiresIn: 3600, Scope: requestedScopes,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	auth := newAuth(credentialPath, filepath.Join(directory, "host"), io.Discard)
	auth.discoveryURL = server.URL + "/discovery"
	auth.now = func() time.Time { return now }
	for range 2 {
		token, err := auth.token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if token != "new-access" {
			t.Fatalf("token = %q", token)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d", refreshes.Load())
	}
	stored, err := readCredentials(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "new-refresh" {
		t.Fatalf("stored credentials = %#v", stored)
	}
}

func TestLogoutClearsTokensWhenRevocationFails(t *testing.T) {
	directory := t.TempDir()
	credentialPath := filepath.Join(directory, "credentials")
	cred := credentials{
		Email: "user@example.com", Issuer: openAIIssuer, Subject: "subject-1", ClientID: "oaiapp_issued",
		AccessToken: "access", RefreshToken: "refresh",
		ExpiresIn: 3600, Scopes: strings.Fields(requestedScopes), SavedAt: time.Now().UTC(),
	}
	if err := writeCredentials(credentialPath, cred); err != nil {
		t.Fatal(err)
	}

	var revocations atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery":
			writeAuthJSON(t, w, testProvider(server.URL))
		case "/revoke":
			revocations.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.PostForm.Get("token") != "refresh" || r.PostForm.Get("token_type_hint") != "refresh_token" || r.PostForm.Get("client_id") != cred.ClientID {
				t.Errorf("revocation form = %v", r.PostForm)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	auth := newAuth(credentialPath, filepath.Join(directory, "host"), io.Discard)
	auth.discoveryURL = server.URL + "/discovery"
	auth.sleep = func(context.Context, time.Duration) error { return nil }
	err := auth.logout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remote revocation was not confirmed") {
		t.Fatalf("logout error = %v", err)
	}
	if revocations.Load() != 3 {
		t.Fatalf("revocation count = %d", revocations.Load())
	}
	stored, readErr := readCredentials(credentialPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if stored.AccessToken != "" || stored.RefreshToken != "" {
		t.Fatalf("tokens were not cleared: %#v", stored)
	}
	if stored.ClientID != cred.ClientID || stored.Subject != cred.Subject || !hasScope(stored.Scopes, directScope) {
		t.Fatalf("registration was not retained: %#v", stored)
	}
}

func TestVerifyIDToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	key := testRSAKey(t)
	const keyID = "test-key"
	keys := testJWKS(&key.PublicKey, keyID)
	claims := map[string]any{
		"iss": openAIIssuer, "sub": "subject-1", "aud": "oaiapp_issued",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": "nonce",
	}
	token := signTestJWT(t, key, keyID, claims)
	if _, err := verifyIDToken(token, "oaiapp_issued", "nonce", openAIIssuer, keys, now); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		clientID string
		nonce    string
		now      time.Time
		token    string
	}{
		{"audience", "other-client", "nonce", now, token},
		{"nonce", "oaiapp_issued", "other-nonce", now, token},
		{"expired", "oaiapp_issued", "nonce", now.Add(2 * time.Hour), token},
		{"signature", "oaiapp_issued", "nonce", now, corruptJWT(t, token)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := verifyIDToken(tt.token, tt.clientID, tt.nonce, openAIIssuer, keys, tt.now); err == nil {
				t.Fatal("verification unexpectedly succeeded")
			}
		})
	}
}

func TestLoginRejectsWrongStateBeforeExchange(t *testing.T) {
	var tokenCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery":
			writeAuthJSON(t, w, testProvider(server.URL))
		case "/token":
			tokenCalls.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	directory := t.TempDir()
	credentialPath := filepath.Join(directory, "credentials")
	auth := newAuth(credentialPath, filepath.Join(directory, "host"), io.Discard)
	auth.discoveryURL = server.URL + "/discovery"
	auth.presentURL = func(address string) error {
		authorizationURL, err := url.Parse(address)
		if err != nil {
			return err
		}
		callback, err := url.Parse(authorizationURL.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		callback.RawQuery = url.Values{"code": {"code"}, "state": {"wrong"}, "client_id": {"oaiapp_issued"}}.Encode()
		response, err := http.Get(callback.String())
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		return err
	}
	if _, err := auth.login(context.Background()); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("login error = %v", err)
	}
	if tokenCalls.Load() != 0 {
		t.Fatalf("token endpoint calls = %d", tokenCalls.Load())
	}
	if _, err := os.Stat(credentialPath); !os.IsNotExist(err) {
		t.Fatalf("credential file exists after failed login: %v", err)
	}
}

func testProvider(serverURL string) oidcMetadata {
	return oidcMetadata{
		Issuer:                openAIIssuer,
		AuthorizationEndpoint: serverURL + "/authorize",
		TokenEndpoint:         serverURL + "/token",
		RevocationEndpoint:    serverURL + "/revoke",
		JWKSURI:               serverURL + "/jwks",
		SigningAlgorithms:     []string{"RS256"},
	}
}

func cloneValues(values url.Values) url.Values {
	copy := make(url.Values, len(values))
	for key, items := range values {
		copy[key] = append([]string(nil), items...)
	}
	return copy
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testJWKS(key *rsa.PublicKey, keyID string) jwkSet {
	exponent := big.NewInt(int64(key.E)).Bytes()
	return jwkSet{Keys: []jwk{{
		KeyType: "RSA", KeyID: keyID, Use: "sig", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(exponent),
	}}}
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(encodedHeader + "." + encodedPayload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return encodedHeader + "." + encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func corruptJWT(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	return strings.Join(parts, ".")
}

func writeAuthJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Error(err)
	}
}
