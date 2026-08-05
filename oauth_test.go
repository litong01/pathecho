package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthProviderSupportsFourGrantTypes(t *testing.T) {
	router := newRouter(newMemoryStore())
	setupOAuthProvider(t, router)

	t.Run("discovery and JWKS", func(t *testing.T) {
		response := performRequest(router, http.MethodGet, "/oauth/.well-known/openid-configuration", "")
		if response.Code != http.StatusOK {
			t.Fatalf("discovery status = %d, body = %s", response.Code, response.Body.String())
		}
		var discovery struct {
			Issuer        string   `json:"issuer"`
			TokenEndpoint string   `json:"token_endpoint"`
			Grants        []string `json:"grant_types_supported"`
		}
		decodeResponse(t, response, &discovery)
		if discovery.Issuer != "http://issuer.example/oauth" ||
			discovery.TokenEndpoint != "http://issuer.example/oauth/token" ||
			len(discovery.Grants) != 4 {
			t.Fatalf("unexpected discovery: %+v", discovery)
		}
		response = performRequest(router, http.MethodGet, "/.well-known/oauth-authorization-server/oauth", "")
		if response.Code != http.StatusOK {
			t.Fatalf("authorization server metadata status = %d, body = %s", response.Code, response.Body.String())
		}

		response = performRequest(router, http.MethodGet, "/oauth/jwks", "")
		if response.Code != http.StatusOK {
			t.Fatalf("JWKS status = %d, body = %s", response.Code, response.Body.String())
		}
		_ = publicKeyFromJWKS(t, response.Body.Bytes())
	})

	t.Run("client credentials", func(t *testing.T) {
		response := performTokenRequest(router, url.Values{
			"grant_type":    {grantClientCredentials},
			"client_id":     {"test-client"},
			"client_secret": {"test-secret"},
			"scope":         {"api.read"},
		}, false)
		token := decodeTokenResponse(t, response)
		if token.RefreshToken != "" || token.IDToken != "" {
			t.Fatalf("client credentials unexpectedly returned user tokens: %+v", token)
		}
		claims := verifyAccessToken(t, router, token.AccessToken)
		if claims["sub"] != "test-client" || claims["aud"] != "test-api" ||
			claims["scope"] != "api.read" || claims["tenant"] != "test-tenant" {
			t.Fatalf("unexpected client credentials claims: %+v", claims)
		}
	})

	var refreshToken string
	t.Run("password", func(t *testing.T) {
		response := performTokenRequest(router, url.Values{
			"grant_type":    {grantPassword},
			"client_id":     {"test-client"},
			"client_secret": {"test-secret"},
			"username":      {"alice"},
			"password":      {"alice-password"},
			"scope":         {"openid profile"},
		}, false)
		token := decodeTokenResponse(t, response)
		refreshToken = token.RefreshToken
		if refreshToken == "" || token.IDToken == "" {
			t.Fatalf("password grant did not return refresh and ID tokens: %+v", token)
		}
		claims := verifyAccessToken(t, router, token.AccessToken)
		if claims["sub"] != "user-alice" || claims["email"] != "alice@example.com" {
			t.Fatalf("unexpected password claims: %+v", claims)
		}
	})

	t.Run("refresh token", func(t *testing.T) {
		response := performTokenRequest(router, url.Values{
			"grant_type":    {grantRefreshToken},
			"client_id":     {"test-client"},
			"client_secret": {"test-secret"},
			"refresh_token": {refreshToken},
			"scope":         {"openid"},
		}, false)
		token := decodeTokenResponse(t, response)
		if token.RefreshToken != refreshToken || token.IDToken == "" {
			t.Fatalf("refresh response did not retain refresh token or issue ID token: %+v", token)
		}
		claims := verifyAccessToken(t, router, token.AccessToken)
		if claims["sub"] != "user-alice" || claims["scope"] != "openid" {
			t.Fatalf("unexpected refreshed claims: %+v", claims)
		}
	})

	t.Run("authorization code with PKCE", func(t *testing.T) {
		verifier := "test-code-verifier"
		challengeHash := sha256.Sum256([]byte(verifier))
		query := url.Values{
			"response_type":         {"code"},
			"client_id":             {"test-client"},
			"redirect_uri":          {"http://client.example/callback"},
			"scope":                 {"openid profile"},
			"state":                 {"request-state"},
			"nonce":                 {"request-nonce"},
			"login_hint":            {"bob"},
			"code_challenge":        {base64.RawURLEncoding.EncodeToString(challengeHash[:])},
			"code_challenge_method": {"S256"},
		}
		response := performRequest(router, http.MethodGet, "/oauth/authorize?"+query.Encode(), "")
		if response.Code != http.StatusFound {
			t.Fatalf("authorize status = %d, body = %s", response.Code, response.Body.String())
		}
		location, err := url.Parse(response.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		code := location.Query().Get("code")
		if code == "" || location.Query().Get("state") != "request-state" {
			t.Fatalf("invalid authorization redirect: %s", location)
		}

		form := url.Values{
			"grant_type":    {grantAuthorizationCode},
			"code":          {code},
			"redirect_uri":  {"http://client.example/callback"},
			"code_verifier": {verifier},
		}
		response = performTokenRequest(router, form, true)
		token := decodeTokenResponse(t, response)
		claims := verifyAccessToken(t, router, token.AccessToken)
		if claims["sub"] != "user-bob" || claims["role"] != "viewer" {
			t.Fatalf("unexpected authorization code claims: %+v", claims)
		}
		idClaims := verifyJWT(t, router, token.IDToken)
		if idClaims["nonce"] != "request-nonce" {
			t.Fatalf("ID token nonce = %v", idClaims["nonce"])
		}

		response = performTokenRequest(router, form, true)
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), `"invalid_grant"`) {
			t.Fatalf("reused code status = %d, body = %s", response.Code, response.Body.String())
		}

		query.Set("scope", "not-allowed")
		query.Set("state", "error-state")
		response = performRequest(router, http.MethodGet, "/oauth/authorize?"+query.Encode(), "")
		if response.Code != http.StatusFound {
			t.Fatalf("authorize error status = %d, body = %s", response.Code, response.Body.String())
		}
		errorLocation, err := url.Parse(response.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if errorLocation.Query().Get("error") != "invalid_scope" ||
			errorLocation.Query().Get("state") != "error-state" {
			t.Fatalf("invalid authorize error redirect: %s", errorLocation)
		}
	})
}

func TestOAuthSetupResetAndErrors(t *testing.T) {
	router := newRouter(newMemoryStore())

	response := performRequest(router, http.MethodGet, "/oauth/jwks", "")
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"oauth_not_configured"`) {
		t.Fatalf("unconfigured JWKS status = %d, body = %s", response.Code, response.Body.String())
	}
	response = performRequest(router, http.MethodGet, "/oauth/token", "")
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET token status = %d, Allow = %q", response.Code, response.Header().Get("Allow"))
	}
	response = performRequest(router, http.MethodPost, "/oauth/jwks?DO=setup", `{}`)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST JWKS status = %d, body = %s", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, router, http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":        "http://issuer.example/oauth",
		"privateKeyPEM": "not a key",
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid key setup status = %d, body = %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(t, router, http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":        "http://issuer.example/oauth",
		"enabledGrants": []string{},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty grants setup status = %d, body = %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(t, router, http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer": "http://issuer.example/oauth",
		"clients": map[string]any{
			"client": map[string]any{
				"redirectURIs": []string{"http://client.example/callback#fragment"},
			},
		},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("fragment redirect setup status = %d, body = %s", response.Code, response.Body.String())
	}

	setupOAuthProvider(t, router)
	response = performTokenRequest(router, url.Values{
		"grant_type":    {"unknown"},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
	}, false)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"unsupported_grant_type"`) {
		t.Fatalf("unknown grant status = %d, body = %s", response.Code, response.Body.String())
	}

	response = performRequest(router, http.MethodPost, "/oauth?DO=reset", "")
	if response.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", response.Code, response.Body.String())
	}
	response = performRequest(router, http.MethodGet, "/oauth/jwks", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("JWKS after reset status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestOAuthSetupAcceptsImportedRSAKeyAndClientShorthand(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}

	router := newRouter(newMemoryStore())
	response := performJSONRequest(t, router, http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":        "http://issuer.example/oauth",
		"privateKeyPEM": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})),
		"clients":       map[string]string{"test-client": "test-secret"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("OAuth setup status = %d, body = %s", response.Code, response.Body.String())
	}

	response = performRequest(router, http.MethodGet, "/oauth/jwks", "")
	publicKey := publicKeyFromJWKS(t, response.Body.Bytes())
	if publicKey.N.Cmp(privateKey.N) != 0 || publicKey.E != privateKey.E {
		t.Fatal("JWKS does not contain the imported key")
	}

	response = performTokenRequest(router, url.Values{
		"grant_type":    {grantClientCredentials},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
		"scope":         {"api.read"},
	}, false)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"invalid_scope"`) {
		t.Fatalf("unconfigured scope status = %d, body = %s", response.Code, response.Body.String())
	}
}

func setupOAuthProvider(t *testing.T, router http.Handler) {
	t.Helper()
	response := performJSONRequest(t, router, http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":   "http://issuer.example/oauth",
		"audience": "test-api",
		"tokenTTL": "15m",
		"claims": map[string]any{
			"tenant": "test-tenant",
		},
		"defaultUser": "alice",
		"clients": map[string]any{
			"test-client": map[string]any{
				"secret":       "test-secret",
				"redirectURIs": []string{"http://client.example/callback"},
				"scopes":       []string{"openid", "profile", "api.read"},
			},
		},
		"users": map[string]any{
			"alice": map[string]any{
				"password": "alice-password",
				"claims": map[string]any{
					"sub":   "user-alice",
					"email": "alice@example.com",
					"role":  "admin",
				},
			},
			"bob": map[string]any{
				"password": "bob-password",
				"claims": map[string]any{
					"sub":   "user-bob",
					"email": "bob@example.com",
					"role":  "viewer",
				},
			},
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("OAuth setup status = %d, body = %s", response.Code, response.Body.String())
	}
}

type testTokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func performTokenRequest(handler http.Handler, form url.Values, basicAuth bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicAuth {
		request.SetBasicAuth("test-client", "test-secret")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeTokenResponse(t *testing.T, response *httptest.ResponseRecorder) testTokenResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("token status = %d, body = %s", response.Code, response.Body.String())
	}
	var token testTokenResponse
	decodeResponse(t, response, &token)
	if token.AccessToken == "" || token.TokenType != "Bearer" || token.ExpiresIn != 900 {
		t.Fatalf("invalid token response: %+v", token)
	}
	return token
}

func verifyAccessToken(t *testing.T, router http.Handler, token string) map[string]any {
	t.Helper()
	return verifyJWT(t, router, token)
}

func verifyJWT(t *testing.T, router http.Handler, token string) map[string]any {
	t.Helper()
	response := performRequest(router, http.MethodGet, "/oauth/jwks", "")
	if response.Code != http.StatusOK {
		t.Fatalf("JWKS status = %d, body = %s", response.Code, response.Body.String())
	}
	publicKey := publicKeyFromJWKS(t, response.Body.Bytes())

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	encodedHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(encodedHeader, &header); err != nil {
		t.Fatal(err)
	}
	var keySet struct {
		Keys []struct {
			Algorithm string `json:"alg"`
			KeyID     string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &keySet); err != nil {
		t.Fatal(err)
	}
	if len(keySet.Keys) != 1 || header.Algorithm != "RS256" ||
		header.KeyID == "" || header.KeyID != keySet.Keys[0].KeyID ||
		keySet.Keys[0].Algorithm != "RS256" {
		t.Fatalf("JWT header does not match JWKS: header=%+v keys=%+v", header, keySet.Keys)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("JWT signature verification failed: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	issuedAt, issuedOK := claims["iat"].(float64)
	expiresAt, expiresOK := claims["exp"].(float64)
	if claims["iss"] != "http://issuer.example/oauth" ||
		!issuedOK || !expiresOK || expiresAt <= issuedAt {
		t.Fatalf("invalid standard JWT claims: %+v", claims)
	}
	return claims
}

func publicKeyFromJWKS(t *testing.T, data []byte) *rsa.PublicKey {
	t.Helper()
	var document struct {
		Keys []struct {
			N string `json:"n"`
			E string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Keys) != 1 {
		t.Fatalf("JWKS has %d keys", len(document.Keys))
	}
	modulus, err := base64.RawURLEncoding.DecodeString(document.Keys[0].N)
	if err != nil {
		t.Fatal(err)
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(document.Keys[0].E)
	if err != nil {
		t.Fatal(err)
	}
	exponent := 0
	for _, value := range exponentBytes {
		exponent = exponent<<8 | int(value)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v\n%s", err, response.Body.String())
	}
}
