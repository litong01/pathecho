//go:build e2e

package e2e

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

/*
Example: configure the in-memory OAuth/OIDC test provider (README "Test OAuth provider").

	curl -X POST 'http://localhost:9095/oauth?DO=setup' \
	  -H 'Content-Type: application/json' \
	  -d '{ ... issuer, clients, users ... }'
*/
func setupOAuth(t *testing.T) {
	t.Helper()
	resetServer(t)

	resp := doJSON(t, http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":      baseURL + "/oauth",
		"audience":    "test-api",
		"tokenTTL":    "1h",
		"defaultUser": "alice",
		"claims":      map[string]any{"tenant": "test"},
		"clients": map[string]any{
			"test-client": map[string]any{
				"secret":       "test-secret",
				"redirectURIs": []string{"http://localhost:3000/callback"},
				"scopes":       []string{"openid", "profile", "api.read"},
			},
			"public-client": map[string]any{
				"redirectURIs": []string{"http://localhost:3000/public-callback"},
				"scopes":       []string{"openid", "profile"},
			},
		},
		"users": map[string]any{
			"alice": map[string]any{
				"password": "alice-password",
				"claims": map[string]any{
					"sub":   "user-alice",
					"email": "alice@example.com",
					"roles": []string{"admin"},
				},
			},
		},
	})
	mustStatus(t, resp, http.StatusCreated)
	containsJSON(t, resp.Body, `"status":"Setup"`)
}

func TestOAuthDiscoveryAndJWKS(t *testing.T) {
	setupOAuth(t)

	discovery := doRequest(t, http.MethodGet, "/oauth/.well-known/openid-configuration", "", nil)
	mustStatus(t, discovery, http.StatusOK)
	var meta struct {
		Issuer        string   `json:"issuer"`
		TokenEndpoint string   `json:"token_endpoint"`
		JWKSURI       string   `json:"jwks_uri"`
		Grants        []string `json:"grant_types_supported"`
	}
	decodeJSON(t, discovery.Body, &meta)
	if meta.Issuer != baseURL+"/oauth" ||
		meta.TokenEndpoint != baseURL+"/oauth/token" ||
		meta.JWKSURI != baseURL+"/oauth/jwks" ||
		len(meta.Grants) != 4 {
		t.Fatalf("unexpected discovery: %+v", meta)
	}

	asMeta := doRequest(t, http.MethodGet, "/.well-known/oauth-authorization-server/oauth", "", nil)
	mustStatus(t, asMeta, http.StatusOK)

	jwks := doRequest(t, http.MethodGet, "/oauth/jwks", "", nil)
	mustStatus(t, jwks, http.StatusOK)
	_ = publicKeyFromJWKS(t, jwks.Body)
}

/*
Example: client_credentials grant.

	curl -X POST 'http://localhost:9095/oauth/token' \
	  -H 'Content-Type: application/x-www-form-urlencoded' \
	  -d 'grant_type=client_credentials&client_id=test-client&client_secret=test-secret&scope=api.read'
*/
func TestOAuthClientCredentials(t *testing.T) {
	setupOAuth(t)

	resp := doForm(t, "/oauth/token", url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
		"scope":         {"api.read"},
	})
	mustStatus(t, resp, http.StatusOK)

	token := decodeToken(t, resp.Body)
	if token.AccessToken == "" || token.RefreshToken != "" || token.IDToken != "" {
		t.Fatalf("unexpected token response: %+v", token)
	}
	claims := verifyAccessToken(t, token.AccessToken)
	if claims["sub"] != "test-client" || claims["aud"] != "test-api" ||
		claims["scope"] != "api.read" || claims["tenant"] != "test" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

/*
Example: password grant (test-only).

	curl -X POST 'http://localhost:9095/oauth/token' \
	  -d 'grant_type=password&client_id=test-client&client_secret=test-secret&username=alice&password=alice-password&scope=openid profile'
*/
func TestOAuthPasswordGrant(t *testing.T) {
	setupOAuth(t)

	resp := doForm(t, "/oauth/token", url.Values{
		"grant_type":    {"password"},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
		"username":      {"alice"},
		"password":      {"alice-password"},
		"scope":         {"openid profile"},
	})
	mustStatus(t, resp, http.StatusOK)

	token := decodeToken(t, resp.Body)
	if token.AccessToken == "" || token.RefreshToken == "" || token.IDToken == "" {
		t.Fatalf("password grant missing tokens: %+v", token)
	}
	claims := verifyAccessToken(t, token.AccessToken)
	if claims["sub"] != "user-alice" || claims["email"] != "alice@example.com" {
		t.Fatalf("unexpected user claims: %+v", claims)
	}
}

/*
Example: authorization_code + refresh_token.

	# 1) GET /oauth/authorize?...  -> 302 Location with ?code=...
	# 2) POST /oauth/token grant_type=authorization_code&code=...&redirect_uri=...
	# 3) POST /oauth/token grant_type=refresh_token&refresh_token=...
*/
func TestOAuthAuthorizationCodeAndRefresh(t *testing.T) {
	setupOAuth(t)

	query := url.Values{
		"response_type": {"code"},
		"client_id":     {"test-client"},
		"redirect_uri":  {"http://localhost:3000/callback"},
		"scope":         {"openid profile api.read"},
		"state":         {"xyz"},
		"login_hint":    {"alice"},
	}
	authorize := doRequest(t, http.MethodGet, "/oauth/authorize?"+query.Encode(), "", nil)
	mustStatus(t, authorize, http.StatusFound)
	location, err := url.Parse(authorize.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" || location.Query().Get("state") != "xyz" {
		t.Fatalf("authorize redirect = %s", location)
	}

	tokenResp := doForm(t, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:3000/callback"},
	})
	mustStatus(t, tokenResp, http.StatusOK)
	token := decodeToken(t, tokenResp.Body)
	if token.AccessToken == "" || token.RefreshToken == "" || token.IDToken == "" {
		t.Fatalf("code exchange missing tokens: %+v", token)
	}

	refresh := doForm(t, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
		"refresh_token": {token.RefreshToken},
	})
	mustStatus(t, refresh, http.StatusOK)
	refreshed := decodeToken(t, refresh.Body)
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" {
		t.Fatalf("refresh missing tokens: %+v", refreshed)
	}
	claims := verifyAccessToken(t, refreshed.AccessToken)
	if claims["sub"] != "user-alice" {
		t.Fatalf("refreshed claims: %+v", claims)
	}
}

func TestOAuthPublicClientRequiresPKCE(t *testing.T) {
	setupOAuth(t)

	query := url.Values{
		"response_type": {"code"},
		"client_id":     {"public-client"},
		"redirect_uri":  {"http://localhost:3000/public-callback"},
		"scope":         {"openid"},
		"state":         {"public-state"},
	}
	withoutPKCE := doRequest(t, http.MethodGet, "/oauth/authorize?"+query.Encode(), "", nil)
	mustStatus(t, withoutPKCE, http.StatusFound)
	location, err := url.Parse(withoutPKCE.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("error") != "invalid_request" {
		t.Fatalf("expected PKCE required error, got %s", location)
	}

	verifier := "public-client-verifier-value"
	sum := sha256.Sum256([]byte(verifier))
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	query.Set("code_challenge_method", "S256")
	withPKCE := doRequest(t, http.MethodGet, "/oauth/authorize?"+query.Encode(), "", nil)
	mustStatus(t, withPKCE, http.StatusFound)
	location, err = url.Parse(withPKCE.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("PKCE authorize redirect = %s", location)
	}

	tokenResp := doForm(t, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"public-client"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:3000/public-callback"},
		"code_verifier": {verifier},
	})
	mustStatus(t, tokenResp, http.StatusOK)
}

func TestOAuthResetClearsProvider(t *testing.T) {
	setupOAuth(t)
	mustStatus(t, doRequest(t, http.MethodPost, "/oauth?DO=reset", "", nil), http.StatusOK)
	jwks := doRequest(t, http.MethodGet, "/oauth/jwks", "", nil)
	mustStatus(t, jwks, http.StatusServiceUnavailable)
	containsJSON(t, jwks.Body, `"oauth_not_configured"`)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func decodeToken(t *testing.T, body string) tokenResponse {
	t.Helper()
	var token tokenResponse
	decodeJSON(t, body, &token)
	return token
}

func verifyAccessToken(t *testing.T, raw string) map[string]any {
	t.Helper()
	jwks := doRequest(t, http.MethodGet, "/oauth/jwks", "", nil)
	mustStatus(t, jwks, http.StatusOK)
	key := publicKeyFromJWKS(t, jwks.Body)

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT: %s", raw)
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	if header.Alg != "RS256" {
		t.Fatalf("alg = %q", header.Alg)
	}

	hashed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, hashed[:], signature); err != nil {
		t.Fatalf("JWT signature invalid: %v", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func publicKeyFromJWKS(t *testing.T, body string) *rsa.PublicKey {
	t.Helper()
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	decodeJSON(t, body, &jwks)
	if len(jwks.Keys) == 0 {
		t.Fatalf("empty JWKS: %s", body)
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(jwks.Keys[0].N)
	if err != nil {
		t.Fatal(err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwks.Keys[0].E)
	if err != nil {
		t.Fatal(err)
	}
	var eInt int
	for _, b := range eBytes {
		eInt = eInt<<8 + int(b)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: eInt,
	}
}
