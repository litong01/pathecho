package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	grantAuthorizationCode = "authorization_code"
	grantClientCredentials = "client_credentials"
	grantRefreshToken      = "refresh_token"
	grantPassword          = "password"
)

var supportedOAuthGrants = []string{
	grantAuthorizationCode,
	grantClientCredentials,
	grantRefreshToken,
	grantPassword,
}

type oauthClientConfig struct {
	Secret       string   `json:"secret,omitempty"`
	RedirectURIs []string `json:"redirectURIs,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// UnmarshalJSON also accepts "client-id": "client-secret" shorthand.
func (c *oauthClientConfig) UnmarshalJSON(data []byte) error {
	var secret string
	if err := json.Unmarshal(data, &secret); err == nil {
		c.Secret = secret
		return nil
	}
	type clientAlias oauthClientConfig
	var value clientAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*c = oauthClientConfig(value)
	return nil
}

type oauthUserConfig struct {
	Password string         `json:"password,omitempty"`
	Claims   map[string]any `json:"claims,omitempty"`
}

type oauthSetupRequest struct {
	Issuer          string                       `json:"issuer"`
	Audience        string                       `json:"audience,omitempty"`
	TokenTTL        string                       `json:"tokenTTL,omitempty"`
	CodeTTL         string                       `json:"codeTTL,omitempty"`
	RefreshTokenTTL string                       `json:"refreshTokenTTL,omitempty"`
	PrivateKeyPEM   string                       `json:"privateKeyPEM,omitempty"`
	EnabledGrants   *[]string                    `json:"enabledGrants,omitempty"`
	DefaultUser     string                       `json:"defaultUser,omitempty"`
	Claims          map[string]any               `json:"claims,omitempty"`
	Clients         map[string]oauthClientConfig `json:"clients,omitempty"`
	Users           map[string]oauthUserConfig   `json:"users,omitempty"`
}

type oauthConfig struct {
	issuer          string
	audience        string
	tokenTTL        time.Duration
	codeTTL         time.Duration
	refreshTokenTTL time.Duration
	privateKey      *rsa.PrivateKey
	kid             string
	enabledGrants   map[string]bool
	defaultUser     string
	claims          map[string]any
	clients         map[string]oauthClientConfig
	users           map[string]oauthUserConfig
}

type authorizationCode struct {
	clientID    string
	redirectURI string
	username    string
	scope       string
	nonce       string
	challenge   string
	method      string
	expiresAt   time.Time
}

type refreshTokenEntry struct {
	clientID  string
	subject   string
	scope     string
	claims    map[string]any
	expiresAt time.Time
}

type oauthService struct {
	mu            sync.RWMutex
	config        *oauthConfig
	codes         map[string]authorizationCode
	refreshTokens map[string]refreshTokenEntry
}

func newOAuthService() *oauthService {
	return &oauthService{
		codes:         make(map[string]authorizationCode),
		refreshTokens: make(map[string]refreshTokenEntry),
	}
}

func (s *oauthService) handleControl(w http.ResponseWriter, r *http.Request) {
	switch controlAction(r) {
	case "setup":
		s.handleSetup(w, r)
	case "reset":
		s.mu.Lock()
		s.config = nil
		s.codes = make(map[string]authorizationCode)
		s.refreshTokens = make(map[string]refreshTokenEntry)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "Reset", "path": "/oauth"})
	default:
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("DO must be setup or reset"))
	}
}

func (s *oauthService) handleSetup(w http.ResponseWriter, r *http.Request) {
	var request oauthSetupRequest
	if err := decodeJSONBody(w, r, &request, false); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	config, err := buildOAuthConfig(request)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	s.config = config
	s.codes = make(map[string]authorizationCode)
	s.refreshTokens = make(map[string]refreshTokenEntry)
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":                "Setup",
		"issuer":                config.issuer,
		"authorizationEndpoint": config.issuer + "/authorize",
		"tokenEndpoint":         config.issuer + "/token",
		"jwksURI":               config.issuer + "/jwks",
		"kid":                   config.kid,
		"enabledGrants":         enabledGrantList(config),
	})
}

func buildOAuthConfig(request oauthSetupRequest) (*oauthConfig, error) {
	issuer := strings.TrimSuffix(strings.TrimSpace(request.Issuer), "/")
	parsedIssuer, err := url.Parse(issuer)
	if err != nil || (parsedIssuer.Scheme != "http" && parsedIssuer.Scheme != "https") ||
		parsedIssuer.Host == "" || parsedIssuer.RawQuery != "" || parsedIssuer.Fragment != "" {
		return nil, fmt.Errorf("issuer must be an absolute HTTP or HTTPS URL without query or fragment")
	}
	if !strings.HasSuffix(parsedIssuer.Path, "/oauth") {
		return nil, fmt.Errorf("issuer path must end with /oauth")
	}

	tokenTTL, err := parsePositiveDuration(request.TokenTTL, time.Hour, "tokenTTL")
	if err != nil {
		return nil, err
	}
	codeTTL, err := parsePositiveDuration(request.CodeTTL, 5*time.Minute, "codeTTL")
	if err != nil {
		return nil, err
	}
	refreshTTL, err := parsePositiveDuration(request.RefreshTokenTTL, 24*time.Hour, "refreshTokenTTL")
	if err != nil {
		return nil, err
	}

	privateKey, err := loadOrGenerateRSAKey(request.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	kid, err := rsaKeyID(&privateKey.PublicKey)
	if err != nil {
		return nil, err
	}

	enabled := make(map[string]bool, len(supportedOAuthGrants))
	grants := supportedOAuthGrants
	if request.EnabledGrants != nil {
		grants = *request.EnabledGrants
		if len(grants) == 0 {
			return nil, fmt.Errorf("enabledGrants must contain at least one grant")
		}
	}
	for _, grant := range grants {
		if !isSupportedGrant(grant) {
			return nil, fmt.Errorf("unsupported enabled grant %q", grant)
		}
		enabled[grant] = true
	}

	if request.DefaultUser != "" {
		if _, ok := request.Users[request.DefaultUser]; !ok {
			return nil, fmt.Errorf("defaultUser %q is not configured", request.DefaultUser)
		}
	}
	for clientID, client := range request.Clients {
		if strings.TrimSpace(clientID) == "" {
			return nil, fmt.Errorf("client ID cannot be empty")
		}
		for _, redirectURI := range client.RedirectURIs {
			parsed, parseErr := url.Parse(redirectURI)
			if parseErr != nil || parsed.Scheme == "" || parsed.Fragment != "" {
				return nil, fmt.Errorf("client %q has invalid redirect URI %q", clientID, redirectURI)
			}
		}
	}
	for username, user := range request.Users {
		if strings.TrimSpace(username) == "" {
			return nil, fmt.Errorf("username cannot be empty")
		}
		if sub, ok := user.Claims["sub"]; ok {
			if _, valid := sub.(string); !valid {
				return nil, fmt.Errorf("user %q claim sub must be a string", username)
			}
		}
	}

	return &oauthConfig{
		issuer:          issuer,
		audience:        request.Audience,
		tokenTTL:        tokenTTL,
		codeTTL:         codeTTL,
		refreshTokenTTL: refreshTTL,
		privateKey:      privateKey,
		kid:             kid,
		enabledGrants:   enabled,
		defaultUser:     request.DefaultUser,
		claims:          cloneClaims(request.Claims),
		clients:         request.Clients,
		users:           request.Users,
	}, nil
}

func parsePositiveDuration(raw string, fallback time.Duration, name string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return value, nil
}

func loadOrGenerateRSAKey(rawPEM string) (*rsa.PrivateKey, error) {
	if strings.TrimSpace(rawPEM) == "" {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate RSA key: %w", err)
		}
		return key, nil
	}

	block, _ := pem.Decode([]byte(rawPEM))
	if block == nil {
		return nil, fmt.Errorf("privateKeyPEM does not contain a PEM block")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("privateKeyPEM must contain an RSA private key")
		}
	} else {
		parsed, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes)
		if parseErr != nil {
			return nil, fmt.Errorf("privateKeyPEM must contain a PKCS#1 or PKCS#8 RSA private key")
		}
		key = parsed
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RSA private key: %w", err)
	}
	return key, nil
}

func rsaKeyID(key *rsa.PublicKey) (string, error) {
	encoded, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", fmt.Errorf("marshal RSA public key: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return base64.RawURLEncoding.EncodeToString(sum[:12]), nil
}

func (s *oauthService) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	config, ok := s.currentConfig(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                config.issuer,
		"authorization_endpoint":                config.issuer + "/authorize",
		"token_endpoint":                        config.issuer + "/token",
		"jwks_uri":                              config.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 enabledGrantList(config),
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
	})
}

func (s *oauthService) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	config, ok := s.currentConfig(w)
	if !ok {
		return
	}
	publicKey := &config.privateKey.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []any{map[string]any{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": config.kid,
			"n":   base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
		}},
	})
}

func (s *oauthService) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	config, ok := s.currentConfig(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	clientID := query.Get("client_id")
	client, ok := config.clients[clientID]
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "unknown client_id")
		return
	}
	redirectURI := query.Get("redirect_uri")
	if !contains(client.RedirectURIs, redirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is not registered for this client")
		return
	}
	state := query.Get("state")
	if !config.enabledGrants[grantAuthorizationCode] {
		redirectOAuthAuthorizeError(w, r, redirectURI, state, "unsupported_response_type", "authorization_code grant is disabled")
		return
	}
	if query.Get("response_type") != "code" {
		redirectOAuthAuthorizeError(w, r, redirectURI, state, "unsupported_response_type", "response_type must be code")
		return
	}
	scope, err := validateScope(query.Get("scope"), client.Scopes)
	if err != nil {
		redirectOAuthAuthorizeError(w, r, redirectURI, state, "invalid_scope", err.Error())
		return
	}

	username := query.Get("login_hint")
	if username == "" {
		username = query.Get("user")
	}
	if username == "" {
		username = config.defaultUser
	}
	if username == "" && len(config.users) == 1 {
		for onlyUser := range config.users {
			username = onlyUser
		}
	}
	if _, exists := config.users[username]; !exists {
		redirectOAuthAuthorizeError(w, r, redirectURI, state, "access_denied", "a configured user is required")
		return
	}

	challenge := query.Get("code_challenge")
	method := query.Get("code_challenge_method")
	if challenge != "" {
		if method == "" {
			method = "plain"
		}
		if method != "plain" && method != "S256" {
			redirectOAuthAuthorizeError(w, r, redirectURI, state, "invalid_request", "unsupported code_challenge_method")
			return
		}
	}
	code, err := randomOpaqueValue()
	if err != nil {
		redirectOAuthAuthorizeError(w, r, redirectURI, state, "server_error", err.Error())
		return
	}
	s.mu.Lock()
	if s.config != config {
		s.mu.Unlock()
		redirectOAuthAuthorizeError(w, r, redirectURI, state, "temporarily_unavailable", "OAuth configuration changed")
		return
	}
	s.codes[code] = authorizationCode{
		clientID:    clientID,
		redirectURI: redirectURI,
		username:    username,
		scope:       scope,
		nonce:       query.Get("nonce"),
		challenge:   challenge,
		method:      method,
		expiresAt:   time.Now().Add(config.codeTTL),
	}
	s.mu.Unlock()

	location, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "configured redirect URI is invalid")
		return
	}
	values := location.Query()
	values.Set("code", code)
	if state != "" {
		values.Set("state", state)
	}
	location.RawQuery = values.Encode()
	http.Redirect(w, r, location.String(), http.StatusFound)
}

func (s *oauthService) handleToken(w http.ResponseWriter, r *http.Request) {
	config, ok := s.currentConfig(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSetupBodySize)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	grant := r.PostForm.Get("grant_type")
	if !config.enabledGrants[grant] {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant type is not enabled")
		return
	}

	clientID, client, authenticated := authenticateOAuthClient(r, config)
	if !authenticated {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth/token"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	var result map[string]any
	var err error
	switch grant {
	case grantAuthorizationCode:
		result, err = s.exchangeAuthorizationCode(config, clientID, r.PostForm)
	case grantClientCredentials:
		result, err = s.issueClientCredentials(config, clientID, client, r.PostForm)
	case grantRefreshToken:
		result, err = s.exchangeRefreshToken(config, clientID, r.PostForm)
	case grantPassword:
		result, err = s.issuePasswordToken(config, clientID, client, r.PostForm)
	default:
		err = &oauthProtocolError{code: "unsupported_grant_type", description: "grant type is not supported"}
	}
	if err != nil {
		protocolError, ok := err.(*oauthProtocolError)
		if !ok {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		writeOAuthError(w, protocolError.statusCode(), protocolError.code, protocolError.description)
		return
	}
	s.writeTokenResponse(w, config, result)
}

func authenticateOAuthClient(
	r *http.Request,
	config *oauthConfig,
) (string, oauthClientConfig, bool) {
	clientID := r.PostForm.Get("client_id")
	secret := r.PostForm.Get("client_secret")
	if basicID, basicSecret, ok := r.BasicAuth(); ok {
		clientID = basicID
		secret = basicSecret
	}
	client, ok := config.clients[clientID]
	if !ok {
		return "", oauthClientConfig{}, false
	}
	if client.Secret != "" && subtleStringCompare(client.Secret, secret) == false {
		return "", oauthClientConfig{}, false
	}
	return clientID, client, true
}

func (s *oauthService) exchangeAuthorizationCode(
	config *oauthConfig,
	clientID string,
	form url.Values,
) (map[string]any, error) {
	rawCode := form.Get("code")
	s.mu.Lock()
	code, ok := s.codes[rawCode]
	if !ok || time.Now().After(code.expiresAt) {
		delete(s.codes, rawCode)
		s.mu.Unlock()
		return nil, newOAuthProtocolError("invalid_grant", "authorization code is invalid or expired")
	}
	if code.clientID != clientID || code.redirectURI != form.Get("redirect_uri") {
		s.mu.Unlock()
		return nil, newOAuthProtocolError("invalid_grant", "authorization code does not match this request")
	}
	if !verifyPKCE(code, form.Get("code_verifier")) {
		s.mu.Unlock()
		return nil, newOAuthProtocolError("invalid_grant", "PKCE verification failed")
	}
	delete(s.codes, rawCode)
	s.mu.Unlock()

	user := config.users[code.username]
	subject := userSubject(code.username, user)
	return s.issueTokens(
		config,
		clientID,
		subject,
		code.scope,
		user.Claims,
		strings.Fields(code.scope),
		code.nonce,
		true,
		true,
	)
}

func (s *oauthService) issueClientCredentials(
	config *oauthConfig,
	clientID string,
	client oauthClientConfig,
	form url.Values,
) (map[string]any, error) {
	scope, err := validateScope(form.Get("scope"), client.Scopes)
	if err != nil {
		return nil, newOAuthProtocolError("invalid_scope", err.Error())
	}
	return s.issueTokens(config, clientID, clientID, scope, nil, nil, "", false, false)
}

func (s *oauthService) issuePasswordToken(
	config *oauthConfig,
	clientID string,
	client oauthClientConfig,
	form url.Values,
) (map[string]any, error) {
	username := form.Get("username")
	user, ok := config.users[username]
	if !ok || user.Password == "" || !subtleStringCompare(user.Password, form.Get("password")) {
		return nil, newOAuthProtocolError("invalid_grant", "username or password is invalid")
	}
	scope, err := validateScope(form.Get("scope"), client.Scopes)
	if err != nil {
		return nil, newOAuthProtocolError("invalid_scope", err.Error())
	}
	return s.issueTokens(
		config,
		clientID,
		userSubject(username, user),
		scope,
		user.Claims,
		strings.Fields(scope),
		"",
		true,
		true,
	)
}

func (s *oauthService) exchangeRefreshToken(
	config *oauthConfig,
	clientID string,
	form url.Values,
) (map[string]any, error) {
	rawToken := form.Get("refresh_token")
	s.mu.RLock()
	entry, ok := s.refreshTokens[rawToken]
	s.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) || entry.clientID != clientID {
		if ok && time.Now().After(entry.expiresAt) {
			s.mu.Lock()
			delete(s.refreshTokens, rawToken)
			s.mu.Unlock()
		}
		return nil, newOAuthProtocolError("invalid_grant", "refresh token is invalid or expired")
	}
	scope := entry.scope
	if requested := strings.TrimSpace(form.Get("scope")); requested != "" {
		normalized := normalizeScope(requested)
		if !scopeSubset(normalized, scope) {
			return nil, newOAuthProtocolError("invalid_scope", "requested scope exceeds the original grant")
		}
		scope = normalized
	}
	result, err := s.issueTokens(config, clientID, entry.subject, scope, entry.claims, strings.Fields(scope), "", true, false)
	if err == nil {
		result["refresh_token"] = rawToken
	}
	return result, err
}

func (s *oauthService) issueTokens(
	config *oauthConfig,
	clientID string,
	subject string,
	scope string,
	customClaims map[string]any,
	requestedScopes []string,
	nonce string,
	issueIDToken bool,
	issueRefreshToken bool,
) (map[string]any, error) {
	if !s.isCurrentConfig(config) {
		return nil, newOAuthProtocolErrorWithStatus(
			http.StatusServiceUnavailable,
			"temporarily_unavailable",
			"OAuth configuration changed",
		)
	}
	now := time.Now().UTC()
	expiresAt := now.Add(config.tokenTTL)
	claims := cloneClaims(config.claims)
	mergeClaims(claims, customClaims)
	claims["iss"] = config.issuer
	claims["sub"] = subject
	if config.audience != "" {
		claims["aud"] = config.audience
	} else {
		claims["aud"] = clientID
	}
	claims["client_id"] = clientID
	claims["iat"] = now.Unix()
	claims["exp"] = expiresAt.Unix()
	if scope != "" {
		claims["scope"] = scope
	}
	jti, err := randomOpaqueValue()
	if err != nil {
		return nil, err
	}
	claims["jti"] = jti
	accessToken, err := signJWT(config, claims)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int64(config.tokenTTL.Seconds()),
	}
	if scope != "" {
		result["scope"] = scope
	}

	if issueIDToken && contains(requestedScopes, "openid") {
		idClaims := cloneClaims(config.claims)
		mergeClaims(idClaims, customClaims)
		idClaims["iss"] = config.issuer
		idClaims["sub"] = subject
		idClaims["aud"] = clientID
		idClaims["iat"] = now.Unix()
		idClaims["exp"] = expiresAt.Unix()
		if nonce != "" {
			idClaims["nonce"] = nonce
		}
		idToken, signErr := signJWT(config, idClaims)
		if signErr != nil {
			return nil, signErr
		}
		result["id_token"] = idToken
	}

	if issueRefreshToken && config.enabledGrants[grantRefreshToken] {
		refreshToken, randomErr := randomOpaqueValue()
		if randomErr != nil {
			return nil, randomErr
		}
		s.mu.Lock()
		if s.config != config {
			s.mu.Unlock()
			return nil, newOAuthProtocolErrorWithStatus(
				http.StatusServiceUnavailable,
				"temporarily_unavailable",
				"OAuth configuration changed",
			)
		}
		s.refreshTokens[refreshToken] = refreshTokenEntry{
			clientID:  clientID,
			subject:   subject,
			scope:     scope,
			claims:    cloneClaims(customClaims),
			expiresAt: now.Add(config.refreshTokenTTL),
		}
		s.mu.Unlock()
		result["refresh_token"] = refreshToken
	}
	if !s.isCurrentConfig(config) {
		return nil, newOAuthProtocolErrorWithStatus(
			http.StatusServiceUnavailable,
			"temporarily_unavailable",
			"OAuth configuration changed",
		)
	}
	return result, nil
}

func signJWT(config *oauthConfig, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": config.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, config.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *oauthService) currentConfig(w http.ResponseWriter) (*oauthConfig, bool) {
	s.mu.RLock()
	config := s.config
	s.mu.RUnlock()
	if config == nil {
		writeOAuthError(w, http.StatusServiceUnavailable, "oauth_not_configured", "call POST /oauth?DO=setup first")
		return nil, false
	}
	return config, true
}

func (s *oauthService) isCurrentConfig(config *oauthConfig) bool {
	s.mu.RLock()
	current := s.config == config
	s.mu.RUnlock()
	return current
}

func (s *oauthService) writeTokenResponse(
	w http.ResponseWriter,
	config *oauthConfig,
	response map[string]any,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.config != config {
		writeOAuthError(
			w,
			http.StatusServiceUnavailable,
			"temporarily_unavailable",
			"OAuth configuration changed",
		)
		return
	}
	writeOAuthTokenResponse(w, response)
}

func enabledGrantList(config *oauthConfig) []string {
	result := make([]string, 0, len(config.enabledGrants))
	for _, grant := range supportedOAuthGrants {
		if config.enabledGrants[grant] {
			result = append(result, grant)
		}
	}
	return result
}

func isSupportedGrant(grant string) bool {
	for _, candidate := range supportedOAuthGrants {
		if grant == candidate {
			return true
		}
	}
	return false
}

func validateScope(requested string, allowed []string) (string, error) {
	scope := normalizeScope(requested)
	if scope == "" {
		if contains(allowed, "*") {
			return "", nil
		}
		return strings.Join(allowed, " "), nil
	}
	if !contains(allowed, "*") && !scopeSubset(scope, strings.Join(allowed, " ")) {
		return "", fmt.Errorf("requested scope is not allowed for this client")
	}
	return scope, nil
}

func normalizeScope(scope string) string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, item := range strings.Fields(scope) {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return strings.Join(result, " ")
}

func scopeSubset(requested, granted string) bool {
	allowed := make(map[string]bool)
	for _, item := range strings.Fields(granted) {
		allowed[item] = true
	}
	for _, item := range strings.Fields(requested) {
		if !allowed[item] {
			return false
		}
	}
	return true
}

func verifyPKCE(code authorizationCode, verifier string) bool {
	if code.challenge == "" {
		return true
	}
	switch code.method {
	case "plain":
		return subtleStringCompare(code.challenge, verifier)
	case "S256":
		sum := sha256.Sum256([]byte(verifier))
		return subtleStringCompare(code.challenge, base64.RawURLEncoding.EncodeToString(sum[:]))
	default:
		return false
	}
}

func userSubject(username string, user oauthUserConfig) string {
	if subject, ok := user.Claims["sub"].(string); ok && subject != "" {
		return subject
	}
	return username
}

func cloneClaims(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mergeClaims(target, source map[string]any) {
	for key, value := range source {
		target[key] = value
	}
}

func randomOpaqueValue() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func subtleStringCompare(expected, actual string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

type oauthProtocolError struct {
	code        string
	description string
	status      int
}

func (e *oauthProtocolError) Error() string {
	return e.code + ": " + e.description
}

func (e *oauthProtocolError) statusCode() int {
	if e.status != 0 {
		return e.status
	}
	return http.StatusBadRequest
}

func newOAuthProtocolError(code, description string) error {
	return &oauthProtocolError{code: code, description: description}
}

func newOAuthProtocolErrorWithStatus(status int, code, description string) error {
	return &oauthProtocolError{status: status, code: code, description: description}
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, map[string]any{
		"error":             code,
		"error_description": description,
	})
}

func oauthMethodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allowed)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
	}
}

func oauthEndpointNotFound(w http.ResponseWriter, _ *http.Request) {
	writeOAuthError(w, http.StatusNotFound, "invalid_request", "OAuth endpoint not found")
}

func redirectOAuthAuthorizeError(
	w http.ResponseWriter,
	r *http.Request,
	redirectURI string,
	state string,
	code string,
	description string,
) {
	location, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, code, description)
		return
	}
	query := location.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state != "" {
		query.Set("state", state)
	}
	location.RawQuery = query.Encode()
	http.Redirect(w, r, location.String(), http.StatusFound)
}

func writeOAuthTokenResponse(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, response)
}
