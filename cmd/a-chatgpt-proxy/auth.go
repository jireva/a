package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openAIDiscoveryURL = "https://auth.openai.com/.well-known/openid-configuration"
	openAIIssuer       = "https://auth.openai.com"
	openAIResource     = "https://api.openai.com/v1"
	dynamicClientID    = "dynamic_agent_client"
	callbackPath       = "/auth/callback"
	directScope        = "chatgpt.tokens.use.direct"
	requestedScopes    = "openid profile email offline_access resource.invoke " + directScope
)

type credentials struct {
	Email        string    `json:"email,omitempty"`
	Issuer       string    `json:"issuer"`
	Subject      string    `json:"subject"`
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresIn    int64     `json:"expires_in,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	SavedAt      time.Time `json:"saved_at,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type oidcMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningAlgorithms     []string `json:"id_token_signing_alg_values_supported"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyType string `json:"kty"`
	KeyID   string `json:"kid"`
	Use     string `json:"use"`
	Alg     string `json:"alg"`
	N       string `json:"n"`
	E       string `json:"e"`
}

type auth struct {
	credentialsPath string
	hostPath        string
	client          *http.Client
	now             func() time.Time
	random          io.Reader
	presentURL      func(string) error
	discoveryURL    string
	sleep           func(context.Context, time.Duration) error

	mu      sync.Mutex
	current *credentials
}

func newAuth(credentialsPath, hostPath string, output io.Writer) *auth {
	return &auth{
		credentialsPath: credentialsPath,
		hostPath:        hostPath,
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:    time.Now,
		random: rand.Reader,
		presentURL: func(address string) error {
			_, err := fmt.Fprintf(output, "a-chatgpt-proxy: open this URL in a browser:\n%s\n", address)
			return err
		},
		discoveryURL: openAIDiscoveryURL,
		sleep: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

func (a *auth) account() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	cred, err := a.load()
	if err != nil {
		return "", err
	}
	if err := validRegistration(cred); err != nil {
		return "", err
	}
	if cred.Email != "" {
		return cred.Email, nil
	}
	return cred.Subject, nil
}

func (a *auth) login(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var previous *credentials
	cred, err := readCredentials(a.credentialsPath)
	if err == nil {
		if err := validRegistration(cred); err != nil {
			return "", err
		}
		previous = &cred
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	hostID, err := a.loadOrCreateHost()
	if err != nil {
		return "", err
	}
	provider, err := a.discover(ctx)
	if err != nil {
		return "", err
	}

	clientID := dynamicClientID
	if previous != nil {
		clientID = previous.ClientID
	}
	var tokens tokenResponse
	var nonce string
	var savedAt time.Time
	for attempt := 0; attempt < 2; attempt++ {
		result, err := a.authorize(ctx, provider, hostID, clientID, previous)
		if err != nil {
			return "", err
		}
		issuedClientID := clientID
		if clientID == dynamicClientID {
			if result.ClientID == "" || result.ClientID == dynamicClientID {
				return "", errors.New("new registration did not return an issued client ID")
			}
			issuedClientID = result.ClientID
		} else if result.ClientID != "" && result.ClientID != clientID {
			return "", errors.New("authorization returned a different client ID")
		}

		tokens, err = a.exchangeCode(ctx, provider.TokenEndpoint, issuedClientID, result)
		if err == nil {
			clientID = issuedClientID
			nonce = result.Nonce
			savedAt = a.now().UTC()
			break
		}
		var endpointErr *oauthEndpointError
		if !errors.As(err, &endpointErr) || endpointErr.Code != "invalid_grant" || attempt == 1 {
			return "", err
		}
		clientID = issuedClientID
	}
	if err := validateTokenResponse(tokens, true); err != nil {
		return "", err
	}

	keys, err := a.fetchJWKS(ctx, provider.JWKSURI)
	if err != nil {
		return "", err
	}
	identity, err := verifyIDToken(tokens.IDToken, clientID, nonce, provider.Issuer, keys, a.now())
	if err != nil {
		return "", fmt.Errorf("validate ID token: %w", err)
	}
	if previous != nil {
		if previous.Issuer != identity.Issuer || previous.Subject != identity.Subject || previous.ClientID != clientID {
			return "", errors.New("signed-in account does not match the selected registration")
		}
	}

	scopes := strings.Fields(tokens.Scope)
	email := identity.Email
	if email == "" && previous != nil {
		email = previous.Email
	}
	next := credentials{
		Email:        email,
		Issuer:       identity.Issuer,
		Subject:      identity.Subject,
		ClientID:     clientID,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresIn:    tokens.ExpiresIn,
		Scopes:       scopes,
		SavedAt:      savedAt,
	}
	if err := writeCredentials(a.credentialsPath, next); err != nil {
		return "", err
	}
	a.current = &next
	if !hasScope(scopes, directScope) {
		return email, errors.New("ChatGPT plan permission was not granted")
	}
	if email != "" {
		return email, nil
	}
	return identity.Subject, nil
}

func (a *auth) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	cred, err := a.load()
	if err != nil {
		return "", err
	}
	if err := validRegistration(cred); err != nil {
		return "", err
	}
	if !hasScope(cred.Scopes, directScope) {
		return "", errors.New("ChatGPT plan permission is not enabled; run a-chatgpt-proxy login")
	}
	if cred.AccessToken == "" || cred.RefreshToken == "" {
		return "", errors.New("not signed in; run a-chatgpt-proxy login")
	}
	expires := cred.SavedAt.Add(time.Duration(cred.ExpiresIn) * time.Second)
	if cred.ExpiresIn > 0 && a.now().Before(expires.Add(-time.Minute)) {
		return cred.AccessToken, nil
	}

	provider, err := a.discover(ctx)
	if err != nil {
		return "", err
	}
	tokens, err := a.refresh(ctx, provider.TokenEndpoint, cred)
	if err != nil {
		if terminalRefreshError(err) {
			clearTokens(&cred)
			if writeErr := writeCredentials(a.credentialsPath, cred); writeErr != nil {
				return "", errors.Join(err, writeErr)
			}
			a.current = &cred
			return "", fmt.Errorf("%w; run a-chatgpt-proxy login", err)
		}
		return "", err
	}
	if err := validateTokenResponse(tokens, false); err != nil {
		return "", err
	}
	savedAt := a.now().UTC()
	scopes := cred.Scopes
	if tokens.Scope != "" {
		scopes = strings.Fields(tokens.Scope)
	}
	next := cred
	next.AccessToken = tokens.AccessToken
	next.RefreshToken = tokens.RefreshToken
	next.ExpiresIn = tokens.ExpiresIn
	next.Scopes = scopes
	next.SavedAt = savedAt
	if err := writeCredentials(a.credentialsPath, next); err != nil {
		return "", err
	}
	a.current = &next
	if !hasScope(scopes, directScope) {
		return "", errors.New("refreshed token lacks ChatGPT plan permission")
	}
	return next.AccessToken, nil
}

func (a *auth) logout(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	cred, err := a.load()
	if err != nil {
		return err
	}
	if err := validRegistration(cred); err != nil {
		return err
	}

	var remoteErr error
	if cred.RefreshToken != "" {
		provider, err := a.discover(ctx)
		if err != nil {
			remoteErr = err
		} else {
			remoteErr = a.revoke(ctx, provider.RevocationEndpoint, cred)
		}
	}
	clearTokens(&cred)
	if err := writeCredentials(a.credentialsPath, cred); err != nil {
		return errors.Join(remoteErr, err)
	}
	a.current = &cred
	if remoteErr != nil {
		return fmt.Errorf("signed out locally; remote revocation was not confirmed; disconnect a-chatgpt-proxy in ChatGPT Settings: %w", remoteErr)
	}
	return nil
}

func (a *auth) load() (credentials, error) {
	if a.current != nil {
		return *a.current, nil
	}
	cred, err := readCredentials(a.credentialsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, errors.New("not signed in; run a-chatgpt-proxy login")
		}
		return credentials{}, err
	}
	a.current = &cred
	return cred, nil
}

func validRegistration(cred credentials) error {
	if cred.Issuer == "" || cred.Subject == "" || cred.ClientID == "" || cred.ClientID == dynamicClientID {
		return errors.New("credential file does not contain a valid account registration")
	}
	return nil
}

func validateTokenResponse(tokens tokenResponse, requireIDToken bool) error {
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return errors.New("token response omitted access or refresh token")
	}
	if requireIDToken && tokens.IDToken == "" {
		return errors.New("token response omitted ID token")
	}
	if !strings.EqualFold(tokens.TokenType, "Bearer") {
		return fmt.Errorf("unsupported token type %q", tokens.TokenType)
	}
	if tokens.ExpiresIn <= 0 {
		return errors.New("token response has invalid expiry")
	}
	return nil
}

func clearTokens(cred *credentials) {
	cred.AccessToken = ""
	cred.RefreshToken = ""
	cred.ExpiresIn = 0
	cred.SavedAt = time.Time{}
}

func hasScope(scopes []string, want string) bool {
	return slices.Contains(scopes, want)
}

type authorizationResult struct {
	Code        string
	ClientID    string
	Verifier    string
	Nonce       string
	RedirectURI string
}

type callbackResult struct {
	Code     string
	ClientID string
	Err      error
}

func (a *auth) authorize(ctx context.Context, provider oidcMetadata, hostID, clientID string, previous *credentials) (authorizationResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return authorizationResult{}, fmt.Errorf("listen for authorization callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := "http://127.0.0.1:" + strconv.Itoa(port) + callbackPath

	state, err := randomValue(a.random)
	if err != nil {
		listener.Close()
		return authorizationResult{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	nonce, err := randomValue(a.random)
	if err != nil {
		listener.Close()
		return authorizationResult{}, fmt.Errorf("generate OIDC nonce: %w", err)
	}
	verifier, err := randomValue(a.random)
	if err != nil {
		listener.Close()
		return authorizationResult{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	challenge := sha256.Sum256([]byte(verifier))

	query := url.Values{
		"client_id":             {clientID},
		"ext_agent_host_id":     {hostID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {requestedScopes},
		"resource":              {openAIResource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}
	if clientID == dynamicClientID {
		query.Set("agent_name_hint", "a-chatgpt-proxy")
	} else if previous != nil {
		if !hasScope(previous.Scopes, directScope) {
			query.Set("prompt", "consent")
		}
	}
	authorizationURL, err := url.Parse(provider.AuthorizationEndpoint)
	if err != nil {
		listener.Close()
		return authorizationResult{}, fmt.Errorf("parse authorization endpoint: %w", err)
	}
	authorizationURL.RawQuery = query.Encode()

	callback := make(chan callbackResult, 1)
	var once sync.Once
	deliver := func(result callbackResult) {
		once.Do(func() { callback <- result })
	}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		values := request.URL.Query()
		returnedState, err := oneQueryValue(values, "state", true)
		if err != nil || subtle.ConstantTimeCompare([]byte(returnedState), []byte(state)) != 1 {
			http.Error(w, "Sign-in could not be verified.", http.StatusBadRequest)
			deliver(callbackResult{Err: errors.New("authorization state did not match")})
			return
		}
		if oauthError := values.Get("error"); oauthError != "" {
			http.Error(w, "Sign-in was not completed.", http.StatusBadRequest)
			deliver(callbackResult{Err: fmt.Errorf("authorization failed: %s", oauthError)})
			return
		}
		code, err := oneQueryValue(values, "code", true)
		if err != nil {
			http.Error(w, "The authorization code was missing.", http.StatusBadRequest)
			deliver(callbackResult{Err: err})
			return
		}
		returnedClientID, err := oneQueryValue(values, "client_id", false)
		if err != nil {
			http.Error(w, "Sign-in could not be verified.", http.StatusBadRequest)
			deliver(callbackResult{Err: err})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "Sign-in received. You may close this window.\n")
		deliver(callbackResult{Code: code, ClientID: returnedClientID})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	if err := a.presentURL(authorizationURL.String()); err != nil {
		return authorizationResult{}, fmt.Errorf("display authorization URL: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var result callbackResult
	select {
	case result = <-callback:
	case err := <-serveErr:
		if err == nil {
			err = errors.New("callback listener stopped")
		}
		return authorizationResult{}, fmt.Errorf("serve authorization callback: %w", err)
	case <-waitCtx.Done():
		return authorizationResult{}, fmt.Errorf("wait for authorization callback: %w", waitCtx.Err())
	}
	if result.Err != nil {
		return authorizationResult{}, result.Err
	}
	return authorizationResult{
		Code:        result.Code,
		ClientID:    result.ClientID,
		Verifier:    verifier,
		Nonce:       nonce,
		RedirectURI: redirectURI,
	}, nil
}

func oneQueryValue(values url.Values, name string, required bool) (string, error) {
	items, ok := values[name]
	if !ok || len(items) == 0 || len(items) == 1 && items[0] == "" {
		if required {
			return "", fmt.Errorf("authorization callback omitted %s", name)
		}
		return "", nil
	}
	if len(items) != 1 {
		return "", fmt.Errorf("authorization callback repeated %s", name)
	}
	return items[0], nil
}

func randomValue(source io.Reader) (string, error) {
	data := make([]byte, 32)
	if _, err := io.ReadFull(source, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (a *auth) exchangeCode(ctx context.Context, endpoint, clientID string, result authorizationResult) (tokenResponse, error) {
	return a.requestTokens(ctx, endpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {result.Code},
		"code_verifier": {result.Verifier},
		"redirect_uri":  {result.RedirectURI},
		"resource":      {openAIResource},
	})
}

func (a *auth) refresh(ctx context.Context, endpoint string, cred credentials) (tokenResponse, error) {
	return a.requestTokens(ctx, endpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cred.ClientID},
		"refresh_token": {cred.RefreshToken},
		"resource":      {openAIResource},
	})
}

type oauthEndpointError struct {
	Status string
	Code   string
}

func (e *oauthEndpointError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("OAuth endpoint returned %s: %s", e.Status, e.Code)
	}
	return "OAuth endpoint returned " + e.Status
}

func (a *auth) requestTokens(ctx context.Context, endpoint string, form url.Values) (tokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("create token request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(request)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("request OAuth tokens: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("read token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var bodyError struct {
			Code string `json:"error"`
		}
		json.Unmarshal(body, &bodyError)
		return tokenResponse{}, &oauthEndpointError{response.Status, bodyError.Code}
	}
	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return tokenResponse{}, fmt.Errorf("decode token response: %w", err)
	}
	return tokens, nil
}

func terminalRefreshError(err error) bool {
	var endpointErr *oauthEndpointError
	if !errors.As(err, &endpointErr) {
		return false
	}
	switch endpointErr.Code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	default:
		return false
	}
}

func (a *auth) revoke(ctx context.Context, endpoint string, cred credentials) error {
	form := url.Values{
		"token":           {cred.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {cred.ClientID},
	}
	var lastErr error
	for _, delay := range []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond} {
		if delay != 0 {
			if err := a.sleep(ctx, delay); err != nil {
				return err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return fmt.Errorf("create revocation request: %w", err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := a.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			return nil
		}
		lastErr = fmt.Errorf("revocation endpoint returned %s", response.Status)
		if response.StatusCode < 500 {
			break
		}
	}
	return lastErr
}

func (a *auth) discover(ctx context.Context) (oidcMetadata, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.discoveryURL, nil)
	if err != nil {
		return oidcMetadata{}, fmt.Errorf("create OIDC discovery request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return oidcMetadata{}, fmt.Errorf("request OIDC discovery: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return oidcMetadata{}, fmt.Errorf("OIDC discovery returned %s", response.Status)
	}
	body, err := readLimited(response.Body)
	if err != nil {
		return oidcMetadata{}, fmt.Errorf("read OIDC discovery: %w", err)
	}
	var provider oidcMetadata
	if err := json.Unmarshal(body, &provider); err != nil {
		return oidcMetadata{}, fmt.Errorf("decode OIDC discovery: %w", err)
	}
	if provider.Issuer != openAIIssuer {
		return oidcMetadata{}, fmt.Errorf("unexpected OIDC issuer %q", provider.Issuer)
	}
	if provider.AuthorizationEndpoint == "" || provider.TokenEndpoint == "" || provider.RevocationEndpoint == "" || provider.JWKSURI == "" {
		return oidcMetadata{}, errors.New("OIDC discovery omitted an endpoint")
	}
	if !slices.Contains(provider.SigningAlgorithms, "RS256") {
		return oidcMetadata{}, errors.New("OIDC discovery does not support RS256")
	}
	return provider, nil
}

func (a *auth) fetchJWKS(ctx context.Context, endpoint string) (jwkSet, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return jwkSet{}, fmt.Errorf("create JWKS request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return jwkSet{}, fmt.Errorf("request JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return jwkSet{}, fmt.Errorf("JWKS endpoint returned %s", response.Status)
	}
	body, err := readLimited(response.Body)
	if err != nil {
		return jwkSet{}, fmt.Errorf("read JWKS: %w", err)
	}
	var keys jwkSet
	if err := json.Unmarshal(body, &keys); err != nil {
		return jwkSet{}, fmt.Errorf("decode JWKS: %w", err)
	}
	if len(keys.Keys) == 0 {
		return jwkSet{}, errors.New("JWKS contains no keys")
	}
	return keys, nil
}

func readLimited(reader io.Reader) ([]byte, error) {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("response is too large")
	}
	return data, nil
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

type idTokenClaims struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        json.RawMessage `json:"aud"`
	AuthorizedParty string          `json:"azp"`
	ExpiresAt       int64           `json:"exp"`
	IssuedAt        int64           `json:"iat"`
	NotBefore       *int64          `json:"nbf"`
	Nonce           string          `json:"nonce"`
	Email           string          `json:"email"`
}

func verifyIDToken(token, clientID, nonce, issuer string, keys jwkSet, now time.Time) (idTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return idTokenClaims{}, errors.New("ID token is not a three-part JWT")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return idTokenClaims{}, errors.New("ID token has invalid header encoding")
	}
	var header jwtHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return idTokenClaims{}, errors.New("ID token has invalid header JSON")
	}
	if header.Algorithm != "RS256" || header.KeyID == "" {
		return idTokenClaims{}, errors.New("ID token does not use a known RS256 key")
	}

	var selected *jwk
	for i := range keys.Keys {
		key := &keys.Keys[i]
		if key.KeyID != header.KeyID || key.KeyType != "RSA" || key.Use != "" && key.Use != "sig" || key.Alg != "" && key.Alg != "RS256" {
			continue
		}
		if selected != nil {
			return idTokenClaims{}, errors.New("JWKS contains duplicate matching keys")
		}
		selected = key
	}
	if selected == nil {
		return idTokenClaims{}, errors.New("ID token signing key was not found")
	}
	publicKey, err := rsaPublicKey(*selected)
	if err != nil {
		return idTokenClaims{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idTokenClaims{}, errors.New("ID token has invalid signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return idTokenClaims{}, errors.New("ID token signature is invalid")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idTokenClaims{}, errors.New("ID token has invalid payload encoding")
	}
	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return idTokenClaims{}, errors.New("ID token has invalid claims")
	}
	if claims.Issuer != issuer || issuer != openAIIssuer {
		return idTokenClaims{}, errors.New("ID token issuer does not match")
	}
	if claims.Subject == "" || claims.ExpiresAt <= 0 || claims.IssuedAt <= 0 {
		return idTokenClaims{}, errors.New("ID token omits a required claim")
	}
	if claims.Nonce != nonce {
		return idTokenClaims{}, errors.New("ID token nonce does not match")
	}
	audiences, err := parseAudience(claims.Audience)
	if err != nil || !slices.Contains(audiences, clientID) {
		return idTokenClaims{}, errors.New("ID token audience does not match")
	}
	if len(audiences) > 1 && claims.AuthorizedParty != clientID {
		return idTokenClaims{}, errors.New("ID token authorized party does not match")
	}
	const skew = int64(5)
	nowUnix := now.Unix()
	if nowUnix-skew >= claims.ExpiresAt {
		return idTokenClaims{}, errors.New("ID token is expired")
	}
	if claims.NotBefore != nil && nowUnix+skew < *claims.NotBefore {
		return idTokenClaims{}, errors.New("ID token is not yet valid")
	}
	if claims.IssuedAt > nowUnix+skew {
		return idTokenClaims{}, errors.New("ID token was issued in the future")
	}
	return claims, nil
}

func parseAudience(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) == 0 {
		return nil, errors.New("invalid audience")
	}
	for _, audience := range many {
		if audience == "" {
			return nil, errors.New("invalid audience")
		}
	}
	return many, nil
}

func rsaPublicKey(key jwk) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil || len(modulus) == 0 {
		return nil, errors.New("JWKS key has invalid modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil || len(exponentBytes) == 0 {
		return nil, errors.New("JWKS key has invalid exponent")
	}
	exponent := new(big.Int).SetBytes(exponentBytes)
	if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > int64(^uint(0)>>1) {
		return nil, errors.New("JWKS key exponent is out of range")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponent.Int64())}, nil
}

func (a *auth) loadOrCreateHost() (string, error) {
	data, err := os.ReadFile(a.hostPath)
	if err == nil {
		hostID := strings.TrimSpace(string(data))
		if !validHostID(hostID) {
			return "", errors.New("host file does not contain a UUIDv4 host ID")
		}
		return hostID, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read host file: %w", err)
	}
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(a.random, bytes); err != nil {
		return "", fmt.Errorf("generate host ID: %w", err)
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(bytes)
	hostID := "urn:uuid:" + hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
	if err := atomicWrite(a.hostPath, []byte(hostID+"\n")); err != nil {
		return "", fmt.Errorf("write host file: %w", err)
	}
	return hostID, nil
}

func validHostID(value string) bool {
	const prefix = "urn:uuid:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	uuid := value[len(prefix):]
	if len(uuid) != 36 || uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' || uuid[14] != '4' {
		return false
	}
	if !strings.ContainsRune("89ab", rune(uuid[19])) {
		return false
	}
	for i, char := range uuid {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func readCredentials(path string) (credentials, error) {
	info, err := os.Stat(path)
	if err != nil {
		return credentials{}, err
	}
	if !info.Mode().IsRegular() {
		return credentials{}, errors.New("credential path is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return credentials{}, errors.New("credential file must be accessible only by its owner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return credentials{}, fmt.Errorf("read credentials: %w", err)
	}
	var cred credentials
	if err := json.Unmarshal(data, &cred); err != nil {
		return credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	return cred, nil
}

func writeCredentials(path string, cred credentials) error {
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(path, data); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

func atomicWrite(path string, data []byte) (err error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".a-chatgpt-proxy-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		temporary.Close()
		if err != nil {
			os.Remove(temporaryName)
		}
	}()
	if err = temporary.Chmod(0600); err != nil {
		return err
	}
	if _, err = temporary.Write(data); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporaryName, path); err != nil {
		return err
	}
	dir, openErr := os.Open(directory)
	if openErr != nil {
		return openErr
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
