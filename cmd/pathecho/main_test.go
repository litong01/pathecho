package main

import (
	"net/http"
	"testing"
)

func TestHTTPServerHasDefensiveTimeouts(t *testing.T) {
	server := newHTTPServer(":0", http.NewServeMux())
	if server.ReadHeaderTimeout <= 0 ||
		server.ReadTimeout <= 0 ||
		server.WriteTimeout <= 0 ||
		server.IdleTimeout <= 0 {
		t.Fatalf("server timeouts are not fully configured: %+v", server)
	}
}
