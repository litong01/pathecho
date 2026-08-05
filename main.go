package main

import (
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	goslog "golang.org/x/exp/slog"

	"github.com/gorilla/mux"
	"github.com/pathecho/auth"
)

var (
	Logger *goslog.Logger
	opts   goslog.HandlerOptions
)

func init() {
	doLog := os.Getenv("DOLOG")
	// TODO getting configuration parameters of the control,
	// then use these parameters to customize the logger.
	if doLog == "" {
		opts.Level = goslog.LevelError
	} else {
		opts.Level = goslog.LevelInfo
	}
	Logger = goslog.New(goslog.NewJSONHandler(os.Stdout, &opts))
	goslog.SetDefault(Logger)
}

func handlerFunc(w http.ResponseWriter, r *http.Request) {
	t := time.Now()
	formatted := fmt.Sprintf("%d-%02d-%02dT%02d:%02d:%02d.%07dZ",
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond())

	var data []byte
	var err error
	defer r.Body.Close()
	Logger.Info("Header value", "Accept-Encoding", r.Header.Get("Accept-Encoding"))
	Logger.Info("Header value", "Authorization", r.Header.Get("Authorization"))
	if strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		var reader *gzip.Reader
		reader, err = gzip.NewReader(r.Body)
		if err != nil {
			Logger.Error("Cannot create gzip reader", "Error", err.Error())
		} else {
			defer reader.Close()
			data, err = io.ReadAll(reader)
			if err != nil {
				Logger.Error("Cannot read from unzipped body", "Error", err.Error())
			}
		}
	} else {
		data, err = io.ReadAll(r.Body)
		if err != nil {
			Logger.Error("Cannot read from request body", "Error", err.Error())
		}
	}

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "Failed",
			"time":   formatted,
			"error":  err.Error(),
		})
	} else {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "PUT" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		content := `{"status": "Created", "time": "` + formatted + `"}`
		if r.Method == "PUT" {
			content = `{"status": "Updated", "time": "` + formatted + `"}`
		}
		w.Write([]byte(content))
		if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			var jsonData interface{}
			json.Unmarshal(data, &jsonData)
			Logger.Info(r.Method, "path", r.RequestURI, "content", jsonData)
		} else {
			Logger.Info(r.Method, "path", r.RequestURI, "content", string(data))
		}
	}
}

func newRouter(store ResponseStore) *mux.Router {
	r := mux.NewRouter()
	stubs := newStubService(store)

	r.PathPrefix("/healthz").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		formatted := fmt.Sprintf("%d-%02d-%02dT%02d:%02d:%02d.%07dZ",
			t.Year(), t.Month(), t.Day(),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		content := `{"status":"OK","time":"` + formatted + "\"}"
		w.Write([]byte(content))
		Logger.Info("GET", "path", r.RequestURI)
	})

	r.PathPrefix("/version").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := os.Getenv("version")
		w.Header().Set("Content-Type", "application/json")
		content := `{"status":"FAIL"}`
		resp, err := http.Get(target)
		if err != nil || resp.StatusCode != 200 {
			w.Write([]byte(content))
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			w.Write([]byte(content))
			return
		}
		w.Write(data)
	})

	r.Path("/RESET").Methods("POST").HandlerFunc(stubs.handleGlobalReset)

	// If this is to setup to deal with protected resources
	// For protected resouces
	if auth.IsSecurityEnabled() {
		secured := r.PathPrefix("/secured").Subrouter()
		authenticator := auth.New()
		secured.Use(authenticator.Middleware())
		// regardless what method call, always write the request uri back
		// to the body
		secured.Path("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(r.RequestURI))
		})

		r.Path("/api/callback").Methods("GET").HandlerFunc(authenticator.APICallback)
	}

	r.PathPrefix("/").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stubs.serveConfigured(w, r) {
			return
		}
		w.Write([]byte(r.RequestURI))
		Logger.Info("GET", "path", r.RequestURI)
	})

	r.PathPrefix("/").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch controlAction(r) {
		case "setup":
			stubs.handleSetup(w, r)
		case "reset":
			stubs.handlePathReset(w, r)
		default:
			if !stubs.serveConfigured(w, r) {
				handlerFunc(w, r)
			}
		}
	})

	r.PathPrefix("/").Methods("PUT").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stubs.serveConfigured(w, r) {
			handlerFunc(w, r)
		}
	})

	r.PathPrefix("/").Methods("DELETE").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stubs.serveConfigured(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
		Logger.Info("DELETE", "path", r.RequestURI)
	})

	return r
}

func main() {
	r := newRouter(newMemoryStore())

	cert := os.Getenv("TLS_CERT")
	key := os.Getenv("TLS_KEY")
	port := os.Getenv("PORT")
	if len(port) == 0 {
		port = ":8080"
	} else {
		port = ":" + port
	}

	var err error
	if len(cert) > 0 && len(key) > 0 {
		Logger.Info("TLS enabled")
		Logger.Info("Certificate", "cert", cert)
		Logger.Info("Certificate", "key", key)

		cfg := &tls.Config{
			MinVersion:               tls.VersionTLS10,
			MaxVersion:               tls.VersionTLS13,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
			CipherSuites: []uint16{
				// TLS 1.0 - 1.2 chipher suites
				tls.TLS_RSA_WITH_RC4_128_SHA,
				tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
				tls.TLS_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
				tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
				tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				// tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
				// tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,

				// TLS 1.3 cipher suites.
				tls.TLS_AES_128_GCM_SHA256,
				tls.TLS_AES_256_GCM_SHA384,
				tls.TLS_CHACHA20_POLY1305_SHA256,

				// TLS_FALLBACK_SCSV isn't a standard cipher suite but an indicator
				// that the client is doing version fallback. See RFC 7507.
				tls.TLS_FALLBACK_SCSV,
			},
		}
		srv := &http.Server{
			Addr:         port,
			Handler:      r,
			TLSConfig:    cfg,
			TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		}

		err = srv.ListenAndServeTLS(cert, key)
	} else {
		Logger.Info("TLS disabled")
		err = http.ListenAndServe(port, r)
	}

	if err != nil {
		Logger.Error(err.Error())
	}
}
