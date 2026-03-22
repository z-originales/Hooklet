// Package harness provides the shared test infrastructure for all Hooklet e2e tests.
//
// It is responsible for the full lifecycle of a test environment:
// building the service and CLI binaries, spinning up an isolated service
// instance, and tearing it down at the end of each test.
//
// RabbitMQ is NOT started per-test. Instead each test suite (smoke, heavy)
// starts a single shared RabbitMQ via StartSharedRabbit in its TestMain,
// then passes the allocated port to every harness via WithRabbitPort.
// This reduces Docker overhead from N containers to 1 per package.
//
// Each test that calls New gets its own isolated service instance:
// a dedicated temp dir, DB file, Unix socket, and HTTP port. Tests
// share nothing except RabbitMQ and can run in parallel without interference.
//
// Typical usage in a test package:
//
//	// main_test.go
//	var sharedRabbitPort int
//
//	func TestMain(m *testing.M) {
//	    port, cleanup := harness.StartSharedRabbit()
//	    sharedRabbitPort = port
//	    code := m.Run()
//	    cleanup()
//	    os.Exit(code)
//	}
//
//	// any_test.go
//	func TestSomething(t *testing.T) {
//	    h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))
//	    // use h.CLI(), h.Post(), h.ConnectWS(), etc.
//	}
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hooklet/internal/api"

	"github.com/coder/websocket"
)

// ─────────────────────────────────────────────
// Binary build (once per test run, cached)
// ─────────────────────────────────────────────

var (
	buildOnce    sync.Once
	builtSvcPath string
	builtCLIPath string
	buildErr     error
)

// buildBinaries compiles the service and CLI binaries into a shared temp dir.
// Called at most once per go test invocation via sync.Once.
func buildBinaries() (svcPath, cliPath string, err error) {
	buildOnce.Do(func() {
		dir, e := os.MkdirTemp("", "hooklet-e2e-bins-")
		if e != nil {
			buildErr = fmt.Errorf("create bin temp dir: %w", e)
			return
		}

		builtSvcPath = filepath.Join(dir, "hooklet-service.exe")
		builtCLIPath = filepath.Join(dir, "hooklet-cli.exe")

		// Find repo root (the directory that contains go.mod)
		root, e := repoRoot()
		if e != nil {
			buildErr = fmt.Errorf("find repo root: %w", e)
			return
		}

		for _, target := range []struct{ out, pkg string }{
			{builtSvcPath, "./cmd/service"},
			{builtCLIPath, "./cmd/cli"},
		} {
			cmd := exec.Command("go", "build", "-o", target.out, target.pkg)
			cmd.Dir = root
			cmd.Stdout = os.Stderr // build output → test stderr for visibility
			cmd.Stderr = os.Stderr
			if e := cmd.Run(); e != nil {
				buildErr = fmt.Errorf("build %s: %w", target.pkg, e)
				return
			}
		}
	})
	return builtSvcPath, builtCLIPath, buildErr
}

// repoRoot walks up from the current working directory to find go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

// ─────────────────────────────────────────────
// Free port helper
// ─────────────────────────────────────────────

// freePort asks the OS for a free TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// ─────────────────────────────────────────────
// Shared RabbitMQ (one per test package)
// ─────────────────────────────────────────────

// StartSharedRabbit starts a single RabbitMQ container intended to be shared
// by all tests in a package. Call this from TestMain and pass the returned
// port to every harness via WithRabbitPort.
//
// The returned cleanup function must be called after m.Run() returns.
//
//	func TestMain(m *testing.M) {
//	    port, cleanup := harness.StartSharedRabbit()
//	    defer cleanup()
//	    os.Exit(m.Run())
//	}
func StartSharedRabbit() (rabbitPort int, cleanup func()) {
	rabbitPort, err := freePort()
	if err != nil {
		panic(fmt.Sprintf("harness: free port (rabbit): %v", err))
	}
	mgmtPort, err := freePort()
	if err != nil {
		panic(fmt.Sprintf("harness: free port (rabbit mgmt): %v", err))
	}

	root, err := repoRoot()
	if err != nil {
		panic(fmt.Sprintf("harness: repo root: %v", err))
	}
	composeFile := filepath.Join(root, "test", "docker-compose.yml")

	// Use a unique project name so multiple packages can coexist.
	projectID := fmt.Sprintf("hooklet-shared-%d", rabbitPort)

	env := append(os.Environ(),
		fmt.Sprintf("HOOKLET_E2E_RABBIT_PORT=%d", rabbitPort),
		fmt.Sprintf("HOOKLET_E2E_RABBIT_MGMT_PORT=%d", mgmtPort),
	)

	up := exec.Command("docker", "compose",
		"-f", composeFile,
		"-p", projectID,
		"up", "-d", "--wait",
	)
	up.Env = env
	up.Stdout = os.Stderr
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		panic(fmt.Sprintf("harness: docker compose up (shared): %v", err))
	}

	cleanup = func() {
		down := exec.Command("docker", "compose",
			"-f", composeFile,
			"-p", projectID,
			"down", "--volumes",
		)
		down.Stdout = os.Stderr
		down.Stderr = os.Stderr
		down.Run() //nolint:errcheck
	}
	return rabbitPort, cleanup
}

// ─────────────────────────────────────────────
// Options for New
// ─────────────────────────────────────────────

// Option configures the Harness created by New.
type Option func(*harnessConfig)

type harnessConfig struct {
	// rabbitPort is the port of an already-running RabbitMQ to connect to.
	// When 0, New will skip RabbitMQ startup (caller must have started it).
	rabbitPort int
	// extraEnv holds additional KEY=VALUE pairs injected into the service process.
	// They override any default env var set by startService.
	extraEnv []string
}

// WithRabbitPort instructs the harness to connect to an already-running
// RabbitMQ on the given port instead of starting its own container.
// Use this when you start a shared RabbitMQ via StartSharedRabbit in TestMain.
func WithRabbitPort(port int) Option {
	return func(c *harnessConfig) {
		c.rabbitPort = port
	}
}

// WithEnv adds an extra environment variable (KEY=VALUE) to the service
// process. Useful for overriding timeouts or feature flags in individual tests.
//
//	harness.New(t,
//	    harness.WithRabbitPort(sharedRabbitPort),
//	    harness.WithEnv("HOOKLET_WS_AUTH_TIMEOUT", "1"),
//	)
func WithEnv(key, value string) Option {
	return func(c *harnessConfig) {
		c.extraEnv = append(c.extraEnv, fmt.Sprintf("%s=%s", key, value))
	}
}

// ─────────────────────────────────────────────
// Harness definition
// ─────────────────────────────────────────────

// Harness holds all runtime handles for a single test environment.
// Obtain one via New(t, opts...); cleanup is registered automatically.
type Harness struct {
	t          *testing.T
	svcBin     string
	cliBin     string
	serviceURL string // e.g. "http://127.0.0.1:PORT"
	socketPath string // Unix socket path used by CLISocket
	adminToken string // bearer token configured on the service
	svcPort    int
	svcCmd     *exec.Cmd
}

// New creates an isolated Harness for the calling test.
// It builds binaries (once), then starts the service and waits for
// /api/status to return 200. Cleanup is registered automatically via t.Cleanup.
//
// You MUST pass WithRabbitPort(port) unless you want the legacy per-test
// RabbitMQ behaviour (which is kept for backward-compat but is slow).
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()

	cfg := &harnessConfig{}
	for _, o := range opts {
		o(cfg)
	}

	svcBin, cliBin, err := buildBinaries()
	if err != nil {
		t.Fatalf("harness: build binaries: %v", err)
	}

	// Allocate an isolated service port
	svcPort, err := freePort()
	if err != nil {
		t.Fatalf("harness: free port (service): %v", err)
	}

	// Isolated working directory (DB, socket)
	tmpDir, err := os.MkdirTemp("", "hooklet-e2e-")
	if err != nil {
		t.Fatalf("harness: temp dir: %v", err)
	}

	adminToken := fmt.Sprintf("e2e-admin-token-%d", svcPort)

	h := &Harness{
		t:          t,
		svcBin:     svcBin,
		cliBin:     cliBin,
		serviceURL: fmt.Sprintf("http://127.0.0.1:%d", svcPort),
		socketPath: filepath.Join(tmpDir, "hooklet.sock"),
		adminToken: adminToken,
		svcPort:    svcPort,
	}

	// If no shared RabbitMQ port was provided, start a dedicated one.
	// This path is kept for backward-compat but should be avoided in new tests.
	rabbitPort := cfg.rabbitPort
	var rabbitCleanup func()
	if rabbitPort == 0 {
		t.Log("harness: WARNING — no shared RabbitMQ port provided; starting a dedicated container (slow)")
		rabbitPort, rabbitCleanup = h.startDedicatedRabbit()
	}

	// Start service binary
	h.startService(tmpDir, svcPort, rabbitPort, cfg.extraEnv)

	// Register teardown
	t.Cleanup(func() {
		h.stopService()
		os.RemoveAll(tmpDir)
		if rabbitCleanup != nil {
			rabbitCleanup()
		}
	})

	return h
}

// ─────────────────────────────────────────────
// Dedicated RabbitMQ (legacy / fallback)
// ─────────────────────────────────────────────

// startDedicatedRabbit starts a per-test RabbitMQ container (legacy path).
// Returns the allocated AMQP port and a cleanup function.
func (h *Harness) startDedicatedRabbit() (rabbitPort int, cleanup func()) {
	h.t.Helper()

	rabbitPort, err := freePort()
	if err != nil {
		h.t.Fatalf("harness: free port (rabbit): %v", err)
	}
	mgmtPort, err := freePort()
	if err != nil {
		h.t.Fatalf("harness: free port (rabbit mgmt): %v", err)
	}

	root, err := repoRoot()
	if err != nil {
		h.t.Fatalf("harness: repo root: %v", err)
	}
	composeFile := filepath.Join(root, "test", "docker-compose.yml")
	projectID := fmt.Sprintf("hooklet-e2e-%d", h.svcPort)

	env := append(os.Environ(),
		fmt.Sprintf("HOOKLET_E2E_RABBIT_PORT=%d", rabbitPort),
		fmt.Sprintf("HOOKLET_E2E_RABBIT_MGMT_PORT=%d", mgmtPort),
	)

	up := exec.Command("docker", "compose",
		"-f", composeFile,
		"-p", projectID,
		"up", "-d", "--wait",
	)
	up.Env = env
	up.Stdout = os.Stderr
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		h.t.Fatalf("harness: docker compose up: %v", err)
	}

	cleanup = func() {
		down := exec.Command("docker", "compose",
			"-f", composeFile,
			"-p", projectID,
			"down", "--volumes",
		)
		down.Stdout = os.Stderr
		down.Stderr = os.Stderr
		down.Run() //nolint:errcheck
	}
	return rabbitPort, cleanup
}

// ─────────────────────────────────────────────
// Service lifecycle
// ─────────────────────────────────────────────

// startService launches the service binary and waits for /api/status to return 200.
// extraEnv entries are appended last, so they override the defaults.
func (h *Harness) startService(tmpDir string, svcPort, rabbitPort int, extraEnv []string) {
	h.t.Helper()

	env := append(os.Environ(),
		fmt.Sprintf("PORT=%d", svcPort),
		"RABBITMQ_HOST=127.0.0.1",
		fmt.Sprintf("RABBITMQ_PORT=%d", rabbitPort),
		"RABBITMQ_USER=guest",
		"RABBITMQ_PASS=guest",
		fmt.Sprintf("HOOKLET_DB_PATH=%s", filepath.Join(tmpDir, "hooklet.db")),
		fmt.Sprintf("HOOKLET_SOCKET=%s", h.socketPath),
		fmt.Sprintf("HOOKLET_ADMIN_TOKEN=%s", h.adminToken),
		"HOOKLET_LOG_LEVEL=warn", // quiet during tests; set to debug if troubleshooting
	)
	// Append caller overrides AFTER defaults so they take precedence.
	env = append(env, extraEnv...)

	cmd := exec.Command(h.svcBin)
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		h.t.Fatalf("harness: start service: %v", err)
	}
	h.svcCmd = cmd

	// Poll /api/status until the service is up AND RabbitMQ is connected.
	// The service returns 200 even before RabbitMQ is ready (body shows
	// "rabbitmq":"disconnected"), so we must check the body, not just the
	// status code.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.serviceURL + api.RouteStatus)
		if err == nil && resp.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), `"rabbitmq":"connected"`) {
				return
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("harness: service + RabbitMQ did not become ready within 30s")
}

// stopService kills the service process and waits for it to exit.
func (h *Harness) stopService() {
	if h.svcCmd != nil && h.svcCmd.Process != nil {
		h.svcCmd.Process.Kill() //nolint:errcheck
		h.svcCmd.Wait()         //nolint:errcheck
	}
}

// ─────────────────────────────────────────────
// CLI helpers
// ─────────────────────────────────────────────

// CLI runs the admin CLI over TCP using the admin bearer token.
// This is the standard path for remote administration.
func (h *Harness) CLI(args ...string) (string, error) {
	return h.runCLI(map[string]string{
		"HOOKLET_HOST":        "127.0.0.1",
		"HOOKLET_PORT":        fmt.Sprintf("%d", h.svcPort),
		"HOOKLET_ADMIN_TOKEN": h.adminToken,
	}, args...)
}

// CLISocket runs the admin CLI over the Unix socket (no token required).
// This simulates local administration with implicit trust.
func (h *Harness) CLISocket(args ...string) (string, error) {
	return h.runCLI(map[string]string{
		"HOOKLET_SOCKET": h.socketPath,
		// HOOKLET_HOST intentionally omitted — forces socket transport
	}, args...)
}

// CLITCPNoToken runs the CLI over TCP without any admin token.
// Used to assert that unauthenticated requests are rejected.
func (h *Harness) CLITCPNoToken(args ...string) (string, error) {
	return h.runCLI(map[string]string{
		"HOOKLET_HOST": "127.0.0.1",
		"HOOKLET_PORT": fmt.Sprintf("%d", h.svcPort),
		// HOOKLET_ADMIN_TOKEN intentionally omitted
	}, args...)
}

// CLITCPWrongToken runs the CLI over TCP with a deliberate wrong token.
// Used to assert that invalid tokens are rejected.
func (h *Harness) CLITCPWrongToken(args ...string) (string, error) {
	return h.runCLI(map[string]string{
		"HOOKLET_HOST":        "127.0.0.1",
		"HOOKLET_PORT":        fmt.Sprintf("%d", h.svcPort),
		"HOOKLET_ADMIN_TOKEN": "this-is-the-wrong-token",
	}, args...)
}

// runCLI executes the CLI binary with extra env vars merged on top of the
// current process environment. Returns combined stdout+stderr and the error.
func (h *Harness) runCLI(extraEnv map[string]string, args ...string) (string, error) {
	cmd := exec.Command(h.cliBin, args...)

	// Build env: start with current process env, then override with extras.
	// We strip any existing HOOKLET_* / PORT variables first to avoid leakage.
	env := make([]string, 0)
	for _, e := range os.Environ() {
		key := strings.SplitN(e, "=", 2)[0]
		if strings.HasPrefix(key, "HOOKLET_") || key == "PORT" {
			continue
		}
		env = append(env, e)
	}
	for k, v := range extraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	return out.String(), err
}

// ─────────────────────────────────────────────
// HTTP helpers
// ─────────────────────────────────────────────

// ServiceURL returns the full base URL of the running service.
func (h *Harness) ServiceURL() string { return h.serviceURL }

// Post sends an HTTP POST to path with optional JSON body and optional headers.
// Returns the response (caller must close Body) or a fatal error.
func (h *Harness) Post(path string, body any, headers map[string]string) *http.Response {
	h.t.Helper()

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("harness: marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(http.MethodPost, h.serviceURL+path, bodyReader)
	if err != nil {
		h.t.Fatalf("harness: build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("harness: POST %s: %v", path, err)
	}
	return resp
}

// Get sends an HTTP GET to path and returns the response (caller must close Body).
func (h *Harness) Get(path string) *http.Response {
	h.t.Helper()
	resp, err := http.Get(h.serviceURL + path)
	if err != nil {
		h.t.Fatalf("harness: GET %s: %v", path, err)
	}
	return resp
}

// PostWithRetry is like Post but retries on HTTP 503 up to maxAttempts times,
// sleeping retryDelay between attempts. Use this immediately after a WebSocket
// auth to avoid the race where the RabbitMQ queue binding is not yet in place
// when the first publish arrives.
func (h *Harness) PostWithRetry(path string, body any, headers map[string]string, maxAttempts int, retryDelay time.Duration) *http.Response {
	h.t.Helper()
	var resp *http.Response
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			time.Sleep(retryDelay)
		}
		resp = h.Post(path, body, headers)
		if resp.StatusCode != http.StatusServiceUnavailable {
			return resp
		}
		resp.Body.Close()
	}
	// Return the last response (still 503) so the caller can assert on it.
	return h.Post(path, body, headers)
}

// AdminGet sends an authenticated GET to an admin path and returns the response.
func (h *Harness) AdminGet(path string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.serviceURL+path, nil)
	if err != nil {
		h.t.Fatalf("harness: build admin GET: %v", err)
	}
	req.Header.Set(api.HeaderAuthorization, api.BearerPrefix+h.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("harness: admin GET %s: %v", path, err)
	}
	return resp
}

// ─────────────────────────────────────────────
// WebSocket helpers
// ─────────────────────────────────────────────

// WSConn wraps a websocket.Conn with helpers for auth and message exchange.
type WSConn struct {
	t    *testing.T
	Conn *websocket.Conn
}

// ConnectWS opens a WebSocket connection to /ws?topics=<topics>.
// The connection is NOT authenticated yet — call Auth() next.
// The connection is closed automatically via t.Cleanup.
func (h *Harness) ConnectWS(topics ...string) *WSConn {
	h.t.Helper()

	topicList := strings.Join(topics, ",")
	wsURL := "ws://127.0.0.1:" + fmt.Sprintf("%d", h.svcPort) + api.RouteSubscribe + "?topics=" + topicList

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		h.t.Fatalf("harness: WS dial: %v", err)
	}

	wsc := &WSConn{t: h.t, Conn: conn}
	h.t.Cleanup(func() { conn.CloseNow() })
	return wsc
}

// Auth sends the auth message and reads the response.
// Returns the raw JSON auth response for inspection.
func (wsc *WSConn) Auth(token string) map[string]interface{} {
	wsc.t.Helper()

	authMsg, err := json.Marshal(map[string]string{
		"type":  "auth",
		"token": token,
	})
	if err != nil {
		wsc.t.Fatalf("harness: marshal auth msg: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := wsc.Conn.Write(ctx, websocket.MessageText, authMsg); err != nil {
		wsc.t.Fatalf("harness: WS write auth: %v", err)
	}

	_, raw, err := wsc.Conn.Read(ctx)
	if err != nil {
		wsc.t.Fatalf("harness: WS read auth response: %v", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(raw, &resp); err != nil {
		wsc.t.Fatalf("harness: unmarshal auth response: %v", err)
	}
	return resp
}

// ReadMessage waits for one text message from the WebSocket (with a 10s timeout).
// Returns the raw bytes.
func (wsc *WSConn) ReadMessage() []byte {
	wsc.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, msg, err := wsc.Conn.Read(ctx)
	if err != nil {
		wsc.t.Fatalf("harness: WS read message: %v", err)
	}
	return msg
}

// ReadMessageRaw waits for one message but returns (bytes, error) without fatal.
// Useful when testing cases where the connection is expected to close.
func (wsc *WSConn) ReadMessageRaw() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, msg, err := wsc.Conn.Read(ctx)
	return msg, err
}

// ReadMessageRawTimeout waits for one message with a custom timeout.
// Useful for auth-timeout tests where the server closes after a short window.
func (wsc *WSConn) ReadMessageRawTimeout(d time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, msg, err := wsc.Conn.Read(ctx)
	return msg, err
}

// ─────────────────────────────────────────────
// Assertion helpers
// ─────────────────────────────────────────────

// AssertStatus fatals if the response status code does not match expected.
func AssertStatus(t *testing.T, resp *http.Response, expected int) {
	t.Helper()
	if resp.StatusCode != expected {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP %d, got %d: %s", expected, resp.StatusCode, body)
	}
}

// AssertBodyContains fatals if the response body does not contain substr.
// It reads the body and replaces resp.Body with a fresh reader so subsequent
// calls to AssertBodyContains on the same response work correctly.
func AssertBodyContains(t *testing.T, resp *http.Response, substr string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Replace with a fresh reader so the next assertion can read again.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if !strings.Contains(string(body), substr) {
		t.Fatalf("expected body to contain %q, got: %s", substr, body)
	}
}

// AssertOutputContains fatals if the CLI output string does not contain substr.
func AssertOutputContains(t *testing.T, output, substr string) {
	t.Helper()
	if !strings.Contains(output, substr) {
		t.Fatalf("expected CLI output to contain %q, got:\n%s", substr, output)
	}
}

// ─────────────────────────────────────────────
// Extraction helpers (parse CLI output)
// ─────────────────────────────────────────────

// ExtractWebhookHash extracts the hash from a line like:
//
//	Topic URL: /webhook/<hash>
func ExtractWebhookHash(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Topic URL:") {
			parts := strings.Split(line, "/webhook/")
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	t.Fatalf("could not extract webhook hash from output:\n%s", output)
	return ""
}

// ExtractConsumerToken extracts the token from a line like:
//
//	Token: <token>
func ExtractConsumerToken(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Token:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Token:"))
		}
	}
	t.Fatalf("could not extract consumer token from output:\n%s", output)
	return ""
}

// ExtractWebhookID extracts the numeric ID from a line like:
//
//	ID: <number>
func ExtractWebhookID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ID:"))
		}
	}
	t.Fatalf("could not extract webhook ID from output:\n%s", output)
	return ""
}

// ExtractConsumerID extracts the consumer ID from a line like:
//
//	Consumer created: NAME (ID: 3)
func ExtractConsumerID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Consumer created:") {
			// Format: "Consumer created: NAME (ID: N)"
			start := strings.LastIndex(line, "(ID: ")
			if start != -1 {
				rest := line[start+5:]
				end := strings.Index(rest, ")")
				if end != -1 {
					return strings.TrimSpace(rest[:end])
				}
			}
		}
	}
	t.Fatalf("could not extract consumer ID from output:\n%s", output)
	return ""
}
