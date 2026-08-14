package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigureLoggerHonorsDOLOG(t *testing.T) {
	t.Setenv("DOLOG", "")
	logger := configureLogger()
	if logger == nil {
		t.Fatal("expected logger")
	}
	t.Setenv("DOLOG", "1")
	if configureLogger() == nil {
		t.Fatal("expected verbose logger")
	}
}

func TestRunStartsPlainAndTLSServers(t *testing.T) {
	t.Setenv("PORT", "0")
	t.Setenv("TLS_CERT", "")
	t.Setenv("TLS_KEY", "")
	t.Setenv("APIDIR", "")

	originalListen := listenAndServe
	originalTLS := listenAndServeTLS
	t.Cleanup(func() {
		listenAndServe = originalListen
		listenAndServeTLS = originalTLS
	})

	listenAndServe = func(server *http.Server) error {
		if server.Addr != ":8080" {
			t.Fatalf("default addr = %q", server.Addr)
		}
		return nil
	}
	t.Setenv("PORT", "")
	if code := run(); code != 0 {
		t.Fatalf("default port exit = %d", code)
	}

	t.Setenv("PORT", "0")
	listenAndServe = func(server *http.Server) error {
		if server.Addr != ":0" {
			t.Fatalf("addr = %q", server.Addr)
		}
		if server.Handler == nil || server.ReadHeaderTimeout <= 0 {
			t.Fatalf("server not configured: %+v", server)
		}
		return http.ErrServerClosed
	}
	if code := run(); code != 1 {
		t.Fatalf("plain listen error exit = %d", code)
	}

	listenAndServe = func(*http.Server) error { return nil }
	if code := run(); code != 0 {
		t.Fatalf("plain listen success exit = %d", code)
	}

	t.Setenv("TLS_CERT", "only-cert")
	t.Setenv("TLS_KEY", "")
	if code := run(); code != 1 {
		t.Fatalf("mismatched TLS exit = %d", code)
	}

	certFile, keyFile := writeTestCertificate(t)
	t.Setenv("TLS_CERT", certFile)
	t.Setenv("TLS_KEY", keyFile)
	listenAndServeTLS = func(server *http.Server, cert, key string) error {
		if cert != certFile || key != keyFile {
			t.Fatalf("tls files = %q %q", cert, key)
		}
		if server.TLSConfig == nil || server.TLSConfig.MinVersion != tls.VersionTLS10 {
			t.Fatalf("tls config missing: %+v", server.TLSConfig)
		}
		if len(server.TLSNextProto) != 0 {
			t.Fatal("expected empty TLSNextProto map")
		}
		return nil
	}
	if code := run(); code != 0 {
		t.Fatalf("tls success exit = %d", code)
	}

	listenAndServeTLS = func(*http.Server, string, string) error {
		return http.ErrServerClosed
	}
	if code := run(); code != 1 {
		t.Fatalf("tls error exit = %d", code)
	}
}

func TestRunImportsOpenAPIBeforeListen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.yaml"), []byte(`
openapi: 3.0.0
paths:
  /demo:
    get:
      responses:
        "200":
          content:
            application/json:
              example: {"ok":true}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PORT", "0")
	t.Setenv("TLS_CERT", "")
	t.Setenv("TLS_KEY", "")
	t.Setenv("APIDIR", dir)

	originalListen := listenAndServe
	t.Cleanup(func() { listenAndServe = originalListen })

	var handler http.Handler
	listenAndServe = func(server *http.Server) error {
		handler = server.Handler
		return nil
	}
	if code := run(); code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	if handler == nil {
		t.Fatal("expected handler")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/demo", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("imported demo = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRunOpenAPIMissingDirIsNoop(t *testing.T) {
	t.Setenv("PORT", "0")
	t.Setenv("TLS_CERT", "")
	t.Setenv("TLS_KEY", "")
	t.Setenv("APIDIR", filepath.Join(t.TempDir(), "nope"))

	originalListen := listenAndServe
	t.Cleanup(func() { listenAndServe = originalListen })
	listenAndServe = func(*http.Server) error { return nil }
	if code := run(); code != 0 {
		t.Fatalf("missing APIDIR exit = %d", code)
	}
}

func TestMainExitsOnRunFailure(t *testing.T) {
	originalExit := osExit
	originalListen := listenAndServe
	t.Cleanup(func() {
		osExit = originalExit
		listenAndServe = originalListen
	})

	t.Setenv("PORT", "0")
	t.Setenv("TLS_CERT", "")
	t.Setenv("TLS_KEY", "")
	t.Setenv("APIDIR", "")
	listenAndServe = func(*http.Server) error { return http.ErrServerClosed }

	var exitCode int
	osExit = func(code int) { exitCode = code }
	main()
	if exitCode != 1 {
		t.Fatalf("main exit = %d", exitCode)
	}

	listenAndServe = func(*http.Server) error { return nil }
	exitCode = -1
	osExit = func(code int) { exitCode = code }
	main()
	if exitCode != 0 {
		t.Fatalf("main success exit = %d", exitCode)
	}
}

func writeTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pathecho-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
