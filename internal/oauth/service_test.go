package oauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOAuthServiceSetupDiscoveryAuthorizeAndGrants(t *testing.T) {
	service := NewService()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/oauth?DO=wat", nil)
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid DO status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth?DO=setup", strings.NewReader(`{`))
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid setup JSON = %d", recorder.Code)
	}

	for _, issuer := range []string{
		"",
		"ftp://issuer.example/oauth",
		"http://issuer.example/oauth?x=1",
		"http://issuer.example/auth",
	} {
		recorder = httptest.NewRecorder()
		request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{"issuer": issuer})
		service.HandleControl(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("issuer %q status = %d", issuer, recorder.Code)
		}
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":   "http://issuer.example/oauth",
		"tokenTTL": "nope",
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad tokenTTL = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":        "http://issuer.example/oauth",
		"enabledGrants": []string{"implicit"},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unsupported grant setup = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":      "http://issuer.example/oauth",
		"defaultUser": "missing",
		"users":       map[string]any{},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing defaultUser = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer": "http://issuer.example/oauth",
		"clients": map[string]any{
			"": map[string]any{"secret": "x"},
		},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty client id = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer": "http://issuer.example/oauth",
		"users": map[string]any{
			"": map[string]any{"password": "x"},
		},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty username = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer": "http://issuer.example/oauth",
		"users": map[string]any{
			"alice": map[string]any{
				"claims": map[string]any{"sub": 123},
			},
		},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("non-string sub = %d", recorder.Code)
	}

	setupOAuth(t, service)

	recorder = httptest.NewRecorder()
	service.HandleDiscovery(recorder, httptest.NewRequest(http.MethodGet, "/oauth/.well-known/openid-configuration", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("discovery = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.HandleJWKS(recorder, httptest.NewRequest(http.MethodGet, "/oauth/jwks", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("jwks = %d", recorder.Code)
	}

	query := url.Values{
		"response_type": {"code"},
		"client_id":     {"unknown"},
		"redirect_uri":  {"http://client.example/callback"},
	}
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown client authorize = %d", recorder.Code)
	}

	query.Set("client_id", "test-client")
	query.Set("redirect_uri", "http://client.example/other")
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad redirect authorize = %d", recorder.Code)
	}

	query.Set("redirect_uri", "http://client.example/callback")
	query.Set("response_type", "token")
	query.Set("state", "abc")
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "unsupported_response_type") {
		t.Fatalf("bad response_type = %d %s", recorder.Code, recorder.Header().Get("Location"))
	}

	query.Set("response_type", "code")
	query.Set("scope", "admin")
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "invalid_scope") {
		t.Fatalf("bad scope = %d %s", recorder.Code, recorder.Header().Get("Location"))
	}

	query.Set("scope", "openid")
	query.Set("user", "missing")
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "access_denied") {
		t.Fatalf("missing user = %d %s", recorder.Code, recorder.Header().Get("Location"))
	}

	query.Del("user")
	query.Set("code_challenge", "challenge")
	query.Set("code_challenge_method", "unsupported")
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "code_challenge_method") {
		t.Fatalf("bad pkce method = %d %s", recorder.Code, recorder.Header().Get("Location"))
	}

	query.Del("code_challenge")
	query.Del("code_challenge_method")
	query.Set("login_hint", "alice")
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound {
		t.Fatalf("authorize = %d %s", recorder.Code, recorder.Body.String())
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("missing code in %s", location)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://client.example/callback"},
		"client_id":     {"test-client"},
		"client_secret": {"wrong"},
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("bad secret = %d", recorder.Code)
	}

	form.Set("client_secret", "test-secret")
	form.Set("code", "missing")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing code = %d", recorder.Code)
	}

	form.Set("grant_type", "password")
	form.Set("username", "alice")
	form.Set("password", "wrong")
	form.Set("scope", "openid")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_grant") {
		t.Fatalf("bad password = %d %s", recorder.Code, recorder.Body.String())
	}

	form.Set("password", "alice-password")
	form.Set("scope", "admin")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_scope") {
		t.Fatalf("password scope = %d %s", recorder.Code, recorder.Body.String())
	}

	form.Set("scope", "openid")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("password grant = %d %s", recorder.Code, recorder.Body.String())
	}
	var token map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	refresh, _ := token["refresh_token"].(string)
	if refresh == "" {
		t.Fatalf("missing refresh token: %#v", token)
	}

	form = url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {"test-client"},
		"client_secret": {"test-secret"},
		"scope":         {"admin"},
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_scope") {
		t.Fatalf("refresh scope = %d %s", recorder.Code, recorder.Body.String())
	}

	form.Set("refresh_token", "missing")
	form.Del("scope")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing refresh = %d", recorder.Code)
	}

	form = url.Values{
		"grant_type": {"client_credentials"},
		"client_id":  {"public-client"},
		"scope":      {"openid"},
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unauthorized_client") {
		t.Fatalf("public client credentials = %d %s", recorder.Code, recorder.Body.String())
	}

	form = url.Values{
		"grant_type": {"password"},
		"client_id":  {"public-client"},
		"username":   {"alice"},
		"password":   {"alice-password"},
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unauthorized_client") {
		t.Fatalf("public password grant = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	service.HandleControl(recorder, httptest.NewRequest(http.MethodPost, "/oauth?DO=reset", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("reset = %d", recorder.Code)
	}
}

func TestOAuthHelpersAndKeyLoading(t *testing.T) {
	if err := (&oauthProtocolError{code: "a", description: "b"}).Error(); err != "a: b" {
		t.Fatalf("Error() = %q", err)
	}
	if status := (&oauthProtocolError{status: http.StatusTeapot}).statusCode(); status != http.StatusTeapot {
		t.Fatalf("statusCode = %d", status)
	}
	if status := (&oauthProtocolError{}).statusCode(); status != http.StatusBadRequest {
		t.Fatalf("default statusCode = %d", status)
	}
	if err := newOAuthProtocolErrorWithStatus(http.StatusServiceUnavailable, "x", "y"); err == nil {
		t.Fatal("expected protocol error")
	}

	recorder := httptest.NewRecorder()
	EndpointNotFound(recorder, httptest.NewRequest(http.MethodGet, "/oauth/nope", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("EndpointNotFound = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	MethodNotAllowed(http.MethodPost)(recorder, httptest.NewRequest(http.MethodGet, "/oauth", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("MethodNotAllowed = %d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}

	redirectOAuthAuthorizeError(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
		"://bad",
		"state",
		"invalid_request",
		"bad",
	)

	if !verifyPKCE(authorizationCode{}, "anything") {
		t.Fatal("empty challenge should pass")
	}
	if !verifyPKCE(authorizationCode{challenge: "plain", method: "plain"}, "plain") {
		t.Fatal("plain PKCE failed")
	}
	sum := sha256.Sum256([]byte("verifier"))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !verifyPKCE(authorizationCode{challenge: challenge, method: "S256"}, "verifier") {
		t.Fatal("S256 PKCE failed")
	}
	if verifyPKCE(authorizationCode{challenge: "x", method: "other"}, "x") {
		t.Fatal("unknown PKCE method passed")
	}

	if userSubject("alice", oauthUserConfig{}) != "alice" {
		t.Fatal("default subject failed")
	}
	if got := normalizeScope(" b  a b "); got != "b a" {
		t.Fatalf("normalizeScope = %q", got)
	}
	if scope, err := validateScope("", []string{"*"}); err != nil || scope != "" {
		t.Fatalf("wildcard empty scope = %q %v", scope, err)
	}
	if scope, err := validateScope("", []string{"openid", "profile"}); err != nil || scope != "openid profile" {
		t.Fatalf("default scope = %q %v", scope, err)
	}
	if !isSupportedGrant(grantPassword) || isSupportedGrant("implicit") {
		t.Fatal("isSupportedGrant mismatch")
	}
	if got, err := parsePositiveDuration("", time.Second, "x"); err != nil || got != time.Second {
		t.Fatalf("fallback duration = %v %v", got, err)
	}
	if _, err := parsePositiveDuration("0s", time.Second, "x"); err == nil {
		t.Fatal("zero duration accepted")
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := loadOrGenerateRSAKey(string(pkcs1)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrGenerateRSAKey("not-pem"); err == nil {
		t.Fatal("non-pem accepted")
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecBytes, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecBytes})
	if _, err := loadOrGenerateRSAKey(string(ecPEM)); err == nil {
		t.Fatal("ECDSA key accepted")
	}
	if _, err := loadOrGenerateRSAKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nope")}))); err == nil {
		t.Fatal("junk PKCS accepted")
	}

	service := NewService()
	recorder = httptest.NewRecorder()
	service.HandleDiscovery(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured discovery = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured authorize = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.HandleToken(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured token = %d", recorder.Code)
	}
}

func TestAuthorizationCodeDisabledAndSingleUserDefault(t *testing.T) {
	service := NewService()
	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":        "http://issuer.example/oauth",
		"enabledGrants": []string{grantClientCredentials},
		"clients": map[string]any{
			"test-client": map[string]any{
				"secret":       "test-secret",
				"redirectURIs": []string{"http://client.example/callback"},
				"scopes":       []string{"*"},
			},
		},
		"users": map[string]any{
			"only": map[string]any{"password": "pw"},
		},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", recorder.Code, recorder.Body.String())
	}

	query := url.Values{
		"response_type": {"code"},
		"client_id":     {"test-client"},
		"redirect_uri":  {"http://client.example/callback"},
		"state":         {"s"},
	}
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "unsupported_response_type") {
		t.Fatalf("disabled auth code = %d %s", recorder.Code, recorder.Header().Get("Location"))
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer": "http://issuer.example/oauth",
		"clients": map[string]any{
			"test-client": map[string]any{
				"secret":       "test-secret",
				"redirectURIs": []string{"http://client.example/callback"},
				"scopes":       []string{"*"},
			},
			"public-client": map[string]any{
				"redirectURIs": []string{"http://client.example/public"},
				"scopes":       []string{"openid"},
			},
		},
		"users": map[string]any{
			"only": map[string]any{"password": "pw"},
		},
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("single user setup = %d %s", recorder.Code, recorder.Body.String())
	}

	query = url.Values{
		"response_type":         {"code"},
		"client_id":             {"public-client"},
		"redirect_uri":          {"http://client.example/public"},
		"scope":                 {"openid"},
		"state":                 {"s"},
		"code_challenge":        {"plain-challenge"},
		"code_challenge_method": {""},
	}
	recorder = httptest.NewRecorder()
	service.HandleAuthorize(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil))
	if recorder.Code != http.StatusFound {
		t.Fatalf("public authorize = %d %s", recorder.Code, recorder.Body.String())
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://client.example/public"},
		"client_id":     {"public-client"},
		"code_verifier": {"plain-challenge"},
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("public token = %d %s", recorder.Code, recorder.Body.String())
	}

	form.Set("code", code)
	form.Set("code_verifier", "wrong")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	service.HandleToken(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("reused/invalid code = %d", recorder.Code)
	}
}

func setupOAuth(t *testing.T, service *Service) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/oauth?DO=setup", map[string]any{
		"issuer":   "http://issuer.example/oauth",
		"audience": "api",
		"clients": map[string]any{
			"test-client": map[string]any{
				"secret":       "test-secret",
				"redirectURIs": []string{"http://client.example/callback"},
				"scopes":       []string{"openid", "profile"},
			},
			"public-client": map[string]any{
				"redirectURIs": []string{"http://client.example/public"},
				"scopes":       []string{"openid"},
			},
		},
		"users": map[string]any{
			"alice": map[string]any{
				"password": "alice-password",
				"claims":   map[string]any{"sub": "user-alice"},
			},
			"bob": map[string]any{
				"password": "bob-password",
			},
		},
		"defaultUser": "alice",
	})
	service.HandleControl(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", recorder.Code, recorder.Body.String())
	}
}

func jsonRequest(method, target string, body any) *http.Request {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	request := httptest.NewRequest(method, target, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	return request
}
