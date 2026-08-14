package server

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pathecho/internal/httpapi"
	"github.com/pathecho/internal/oauth"
	"github.com/pathecho/internal/stub"
	goslog "golang.org/x/exp/slog"
)

const (
	maxLoggedBodySize = 4 << 10
	versionTimeout    = 5 * time.Second
)

var errBodyTooLarge = errors.New("request body exceeds maximum size")
var errUnsupportedContentEncoding = errors.New("unsupported Content-Encoding")

func readLimited(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, httpapi.MaxBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > httpapi.MaxBodySize {
		return nil, errBodyTooLarge
	}
	return data, nil
}

func logRequestBody(logger *goslog.Logger, r *http.Request, data []byte) {
	if len(data) > maxLoggedBodySize {
		logger.Info(r.Method, "path", r.RequestURI, "content", string(data[:maxLoggedBodySize]), "truncated", true)
		return
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var jsonData any
		if json.Unmarshal(data, &jsonData) == nil {
			logger.Info(r.Method, "path", r.RequestURI, "content", jsonData)
			return
		}
	}
	logger.Info(r.Method, "path", r.RequestURI, "content", string(data))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	t := time.Now()
	formatted := fmt.Sprintf("%d-%02d-%02dT%02d:%02d:%02d.%07dZ",
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond())

	var data []byte
	var err error
	defer r.Body.Close()
	logger := goslog.Default()
	logger.Info("Request headers",
		"Accept-Encoding", r.Header.Get("Accept-Encoding"),
		"AuthorizationPresent", r.Header.Get("Authorization") != "",
	)
	r.Body = http.MaxBytesReader(w, r.Body, httpapi.MaxBodySize)
	contentEncoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if contentEncoding == "gzip" {
		var reader *gzip.Reader
		reader, err = gzip.NewReader(r.Body)
		if err != nil {
			logger.Error("Cannot create gzip reader", "Error", err.Error())
		} else {
			defer reader.Close()
			data, err = readLimited(reader)
			if err != nil {
				logger.Error("Cannot read from unzipped body", "Error", err.Error())
			}
		}
	} else if contentEncoding == "" || contentEncoding == "identity" {
		data, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Error("Cannot read from request body", "Error", err.Error())
		}
	} else {
		err = errUnsupportedContentEncoding
	}

	if err != nil {
		status := http.StatusBadRequest
		var maxBytesError *http.MaxBytesError
		if errors.Is(err, errBodyTooLarge) || errors.As(err, &maxBytesError) {
			status = http.StatusRequestEntityTooLarge
		} else if errors.Is(err, errUnsupportedContentEncoding) {
			status = http.StatusUnsupportedMediaType
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "Failed",
			"time":   formatted,
			"error":  err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPut || r.Method == http.MethodPatch {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	content := `{"status": "Created", "time": "` + formatted + `"}`
	if r.Method == http.MethodPut || r.Method == http.MethodPatch {
		content = `{"status": "Updated", "time": "` + formatted + `"}`
	}
	_, _ = w.Write([]byte(content))
	logRequestBody(logger, r, data)
}

func NewRouter() *mux.Router {
	return NewRouterWith(stub.NewService(), oauth.NewService())
}

// NewRouterWith wires the HTTP surface onto the provided stub and OAuth
// services so callers can preload setups (for example from OpenAPI) before
// serving traffic.
func NewRouterWith(stubs *stub.Service, oauthProvider *oauth.Service) *mux.Router {
	if stubs == nil {
		stubs = stub.NewService()
	}
	if oauthProvider == nil {
		oauthProvider = oauth.NewService()
	}
	r := mux.NewRouter()
	logger := goslog.Default()

	r.Path("/healthz").Methods(http.MethodGet).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		formatted := fmt.Sprintf("%d-%02d-%02dT%02d:%02d:%02d.%07dZ",
			t.Year(), t.Month(), t.Day(),
			t.Hour(), t.Minute(), t.Second(), t.Nanosecond())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK","time":"` + formatted + `"}`))
		logger.Info("GET", "path", r.RequestURI)
	})

	r.Path("/version").Methods(http.MethodGet).HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target := strings.TrimSpace(os.Getenv("version"))
		w.Header().Set("Content-Type", "application/json")
		if target == "" {
			_, _ = w.Write([]byte(`{"status":"FAIL"}`))
			return
		}
		response, err := (&http.Client{Timeout: versionTimeout}).Get(target)
		if err != nil {
			_, _ = w.Write([]byte(`{"status":"FAIL"}`))
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			_, _ = w.Write([]byte(`{"status":"FAIL"}`))
			return
		}
		data, err := readLimited(response.Body)
		if err != nil {
			_, _ = w.Write([]byte(`{"status":"FAIL"}`))
			return
		}
		_, _ = w.Write(data)
	})

	r.Path("/RESET").Methods(http.MethodPost).HandlerFunc(stubs.HandleGlobalReset)

	r.Path("/oauth").Methods(http.MethodPost).HandlerFunc(oauthProvider.HandleControl)
	r.Path("/oauth/authorize").Methods(http.MethodGet).HandlerFunc(oauthProvider.HandleAuthorize)
	r.Path("/oauth/token").Methods(http.MethodPost).HandlerFunc(oauthProvider.HandleToken)
	r.Path("/oauth/jwks").Methods(http.MethodGet).HandlerFunc(oauthProvider.HandleJWKS)
	r.Path("/oauth/.well-known/openid-configuration").Methods(http.MethodGet).HandlerFunc(oauthProvider.HandleDiscovery)
	r.Path("/oauth/.well-known/oauth-authorization-server").Methods(http.MethodGet).HandlerFunc(oauthProvider.HandleDiscovery)
	r.Path("/.well-known/oauth-authorization-server/oauth").Methods(http.MethodGet).HandlerFunc(oauthProvider.HandleDiscovery)

	r.Path("/oauth").HandlerFunc(oauth.MethodNotAllowed(http.MethodPost))
	r.Path("/oauth/authorize").HandlerFunc(oauth.MethodNotAllowed(http.MethodGet))
	r.Path("/oauth/token").HandlerFunc(oauth.MethodNotAllowed(http.MethodPost))
	r.Path("/oauth/jwks").HandlerFunc(oauth.MethodNotAllowed(http.MethodGet))
	r.Path("/oauth/.well-known/openid-configuration").HandlerFunc(oauth.MethodNotAllowed(http.MethodGet))
	r.Path("/oauth/.well-known/oauth-authorization-server").HandlerFunc(oauth.MethodNotAllowed(http.MethodGet))
	r.Path("/.well-known/oauth-authorization-server/oauth").HandlerFunc(oauth.MethodNotAllowed(http.MethodGet))
	r.PathPrefix("/oauth/").HandlerFunc(oauth.EndpointNotFound)

	r.PathPrefix("/").Methods(http.MethodGet).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stubs.ServeConfigured(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(r.RequestURI))
		logger.Info("GET", "path", r.RequestURI)
	})

	r.PathPrefix("/").Methods(http.MethodPost).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch httpapi.ControlAction(r) {
		case "setup":
			stubs.HandleSetup(w, r)
		case "list":
			stubs.HandleList(w, r)
		case "reset":
			stubs.HandlePathReset(w, r)
		default:
			if !stubs.ServeConfigured(w, r) {
				handleRequest(w, r)
			}
		}
	})

	r.PathPrefix("/").Methods(http.MethodPut).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stubs.ServeConfigured(w, r) {
			handleRequest(w, r)
		}
	})

	r.PathPrefix("/").Methods(http.MethodPatch).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stubs.ServeConfigured(w, r) {
			handleRequest(w, r)
		}
	})

	r.PathPrefix("/").Methods(http.MethodDelete).HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stubs.ServeConfigured(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
		logger.Info("DELETE", "path", r.RequestURI)
	})

	return r
}
