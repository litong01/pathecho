//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	defaultImage = "pathecho-e2e:local"
	readyTimeout = 60 * time.Second
)

// Shared across the e2e suite. TestMain boots one container for the whole run.
var (
	baseURL       string
	containerName string
	imageName     string
	httpClient    = &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: docker is required")
		os.Exit(1)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}

	apisDir := filepath.Join(repoRoot, "apis")
	if _, err := os.Stat(filepath.Join(apisDir, "openapi.yaml")); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: apis/openapi.yaml is required:", err)
		os.Exit(1)
	}

	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: allocate port:", err)
		os.Exit(1)
	}
	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	containerName = fmt.Sprintf("pathecho-e2e-%d", os.Getpid())
	imageName = envOr("PATHECHO_E2E_IMAGE", defaultImage)

	cleanup := func() {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	}
	cleanup()

	if os.Getenv("PATHECHO_E2E_SKIP_BUILD") == "" {
		fmt.Fprintf(os.Stderr, "e2e: building image %s\n", imageName)
		build := exec.Command("docker", "build", "-t", imageName, ".")
		build.Dir = repoRoot
		build.Stdout = os.Stderr
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: docker build failed:", err)
			os.Exit(1)
		}
	} else {
		fmt.Fprintf(os.Stderr, "e2e: skipping build; using image %s\n", imageName)
	}

	fmt.Fprintf(os.Stderr, "e2e: starting container %s on %s with APIDIR=/apis\n", containerName, baseURL)
	run := exec.Command(
		"docker", "run", "-d", "--rm",
		"--name", containerName,
		"-p", fmt.Sprintf("%d:8080", port),
		"-e", "APIDIR=/apis",
		"-v", apisDir+":/apis:ro",
		imageName,
	)
	run.Stdout = os.Stderr
	run.Stderr = os.Stderr
	if err := run.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: docker run failed:", err)
		os.Exit(1)
	}

	if err := waitReady(baseURL, readyTimeout); err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		fmt.Fprintf(os.Stderr, "e2e: container not ready: %v\nlogs:\n%s\n", err, logs)
		cleanup()
		os.Exit(1)
	}

	code := m.Run()
	cleanup()
	os.Exit(code)
}

func findRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve caller path")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", file)
		}
		dir = parent
	}
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := httpClient.Do(req)
		cancel()
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("healthz status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func resetServer(t *testing.T) {
	t.Helper()
	mustStatus(t, doRequest(t, http.MethodPost, "/RESET", "", nil), http.StatusOK)
	mustStatus(t, doRequest(t, http.MethodPost, "/oauth?DO=reset", "", nil), http.StatusOK)
}

// reloadServer restarts the shared container so APIDIR OpenAPI setups are
// installed again. Use this for OpenAPI bootstrap tests; resetServer alone
// clears imported setups until the process restarts.
func reloadServer(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "restart", containerName).Run(); err != nil {
		t.Fatalf("docker restart %s: %v", containerName, err)
	}
	if err := waitReady(baseURL, readyTimeout); err != nil {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		t.Fatalf("container not ready after reload: %v\nlogs:\n%s", err, logs)
	}
}
