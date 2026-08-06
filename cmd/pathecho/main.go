package main

import (
	"crypto/tls"
	"net/http"
	"os"
	"time"

	"github.com/pathecho/internal/server"
	goslog "golang.org/x/exp/slog"
)

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func configureLogger() *goslog.Logger {
	options := goslog.HandlerOptions{Level: goslog.LevelError}
	if os.Getenv("DOLOG") != "" {
		options.Level = goslog.LevelInfo
	}
	logger := goslog.New(goslog.NewJSONHandler(os.Stdout, &options))
	goslog.SetDefault(logger)
	return logger
}

func main() {
	logger := configureLogger()
	router := server.NewRouter()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	address := ":" + port

	cert := os.Getenv("TLS_CERT")
	key := os.Getenv("TLS_KEY")
	if (cert == "") != (key == "") {
		logger.Error("TLS_CERT and TLS_KEY must be configured together")
		os.Exit(1)
	}
	httpServer := newHTTPServer(address, router)
	var err error
	if cert != "" && key != "" {
		logger.Info("TLS enabled", "cert", cert, "key", key)
		httpServer.TLSConfig = &tls.Config{
			MinVersion:               tls.VersionTLS10,
			MaxVersion:               tls.VersionTLS13,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
				tls.TLS_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
				tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
				tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
				tls.TLS_RSA_WITH_RC4_128_SHA,
				tls.TLS_AES_128_GCM_SHA256,
				tls.TLS_AES_256_GCM_SHA384,
				tls.TLS_CHACHA20_POLY1305_SHA256,
				tls.TLS_FALLBACK_SCSV,
			},
		}
		httpServer.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
		err = httpServer.ListenAndServeTLS(cert, key)
	} else {
		logger.Info("TLS disabled")
		err = httpServer.ListenAndServe()
	}
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
}
