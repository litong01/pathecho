package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestPruneExpiredOAuthState(t *testing.T) {
	now := time.Now()
	service := NewService()
	service.codes["expired"] = authorizationCode{expiresAt: now.Add(-time.Second)}
	service.codes["active"] = authorizationCode{expiresAt: now.Add(time.Minute)}
	service.refreshTokens["expired"] = refreshTokenEntry{expiresAt: now.Add(-time.Second)}
	service.refreshTokens["active"] = refreshTokenEntry{expiresAt: now.Add(time.Minute)}

	service.mu.Lock()
	service.pruneExpiredLocked(now)
	service.mu.Unlock()

	if len(service.codes) != 1 || len(service.refreshTokens) != 1 {
		t.Fatalf("unexpected state after pruning: codes=%d refresh=%d", len(service.codes), len(service.refreshTokens))
	}
	if _, ok := service.codes["active"]; !ok {
		t.Fatal("active authorization code was removed")
	}
	if _, ok := service.refreshTokens["active"]; !ok {
		t.Fatal("active refresh token was removed")
	}
}

func TestImportedRSAKeyMustBeStrong(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if _, err := loadOrGenerateRSAKey(string(encoded)); err == nil {
		t.Fatal("1024-bit RSA key was accepted")
	}
}

func TestRedirectSchemeFiltering(t *testing.T) {
	for _, scheme := range []string{"", "about", "blob", "javascript", "data", "file", "vbscript"} {
		if safeRedirectScheme(scheme) {
			t.Fatalf("unsafe scheme %q was accepted", scheme)
		}
	}
	for _, scheme := range []string{"http", "https", "com.example.app"} {
		if !safeRedirectScheme(scheme) {
			t.Fatalf("safe scheme %q was rejected", scheme)
		}
	}
}
