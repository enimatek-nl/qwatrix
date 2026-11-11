// Package runtime_test runs the real binary.
//
// The in-process suite starts a homeserver from application code and talks to it over a listener the
// test owns, which is the right shape for testing behaviour and the wrong shape for testing that a
// process starts, finds its files, and stops when asked. Everything here is about the thing an operator
// actually runs: one executable, one directory, two signals.
package runtime_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// binaryPath is the compiled server, built once for the package.
//
// A test that ran `go run` per case would spend more time compiling than running, and a test that
// reached for an installed `qwatrix` in $PATH would be testing whatever happened to be installed.
var binaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "qwatrix-runtime-build")
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not make a build directory: ", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	binaryPath = filepath.Join(tmp, "qwatrix")
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not find the module root: ", err)
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/qwatrix")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the server for the runtime tests: %v\n%s\n", err, out)
		_ = os.RemoveAll(tmp)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// runningServer is a started process plus the pipes its output goes to.
type runningServer struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	baseURL string
}

// start launches the binary in workDir with the given arguments and waits for it to answer
// /_matrix/client/versions.
//
// Waiting for the health endpoint rather than sleeping is the difference between a test that fails on a
// loaded machine and a test that fails when the server is genuinely broken.
func start(t *testing.T, workDir, address string, args ...string) *runningServer {
	t.Helper()
	server := &runningServer{
		t:      t,
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	server.cmd = exec.Command(binaryPath, args...)
	server.cmd.Dir = workDir
	server.cmd.Stdout = server.stdout
	server.cmd.Stderr = server.stderr
	if err := server.cmd.Start(); err != nil {
		t.Fatalf("starting the server: %v", err)
	}
	t.Cleanup(func() {
		// A server still running here is a test that did not stop it; killing it is cleanup, not an
		// assertion, but leaving it would outlive the temporary directory it is writing into.
		if server.cmd.Process != nil && server.running() {
			_ = server.cmd.Process.Kill()
			_, _ = server.cmd.Process.Wait()
		}
	})

	server.baseURL = "http://" + address
	if !waitForHealth(server.baseURL, 20*time.Second) {
		t.Fatalf("the server never answered %s/_matrix/client/versions\nSTDERR:\n%s\nSTDOUT:\n%s",
			server.baseURL, server.stderr.String(), server.stdout.String())
	}
	return server
}

// running reports whether the process is still up, without waiting for it.
func (s *runningServer) running() bool {
	process, err := os.FindProcess(s.cmd.Process.Pid)
	if err != nil {
		return false
	}
	// Signal 0 asks whether the process exists without telling it anything.
	return process.Signal(syscall.Signal(0)) == nil
}

// stop sends a signal and waits for the process to go away, returning how long it took and the wait
// status. It deliberately does not fail the test itself: "did it exit cleanly, and how fast" is the
// measurement, and a measurement reported as a fatal error at the wrong line is harder to read.
func (s *runningServer) stop(signal syscall.Signal, grace time.Duration) (time.Duration, error) {
	if err := s.cmd.Process.Signal(signal); err != nil {
		return 0, fmt.Errorf("sending %s: %w", signal, err)
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		return 0, err
	case <-time.After(grace):
		return grace, errors.New("the process was still running after " + grace.String())
	}
}

// output is everything the process wrote, on either stream.
func (s *runningServer) output() string {
	return s.stderr.String() + s.stdout.String()
}

// waitForHealth polls until the server answers, or the deadline passes.
func waitForHealth(baseURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/_matrix/client/versions")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// freeLoopback returns an address this test just reserved and released, so a machine with something
// else on port 8008 does not decide the outcome.
func freeLoopback(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return address
}

// register makes a user over the running server's own client API, returning the access token.
//
// It checks both halves of the User-Interactive Authentication contract against the real binary: a
// request that names no stage is answered with the flows and a session — which is what drives a
// client's registration form — and a request that already carries the stage it intends to satisfy is
// completed on the spot. The second half is not a shortcut: `m.login.dummy` asks for no credential,
// and complement's blueprint builder registers its users that way, so refusing it once stopped the
// whole server-server suite from starting a homeserver.
func register(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	password := "runtime-test-password"

	post := func(body any) (int, string) {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the registration: %v", err)
		}
		response, err := http.Post(baseURL+"/_matrix/client/v3/register", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("registering: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return response.StatusCode, string(raw)
	}

	status, payload := post(map[string]any{"username": "testuser", "password": password})
	if status != http.StatusUnauthorized {
		t.Fatalf("a registration with no auth answered %d, want the 401 challenge: %s", status, payload)
	}
	var challenge struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal([]byte(payload), &challenge); err != nil || challenge.Session == "" {
		t.Fatalf("the authentication challenge carries no session: %v %s", err, payload)
	}

	status, payload = post(map[string]any{
		"username": "testuser",
		"password": password,
		"auth":     map[string]any{"type": "m.login.dummy"},
	})
	if status != http.StatusOK {
		t.Fatalf("completing registration answered %d, want 200: %s", status, payload)
	}
	var decoded struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decoding the registration response: %v\n%s", err, payload)
	}
	if decoded.AccessToken == "" {
		t.Fatalf("registration returned no access token: %s", payload)
	}
	return decoded.AccessToken, password
}

// TestBootingWithNoArgumentsUsesTheDevelopmentDefaults checks AC-7's first claim literally: an empty
// directory, no flags, no environment, and a working homeserver.
func TestBootingWithNoArgumentsUsesTheDevelopmentDefaults(t *testing.T) {
	dir := t.TempDir()
	server := start(t, dir, "127.0.0.1:8008")

	// The development database has to appear in the working directory, which is what "dev defaults"
	// means: a newcomer runs the binary and gets a server, not a configuration exercise.
	if _, err := os.Stat(filepath.Join(dir, "qwatrix.db")); err != nil {
		t.Fatalf("the default database did not appear in the working directory: %v\n"+
			"the directory holds: %v\nSTDERR:\n%s", err, listing(t, dir), server.output())
	}
	if _, err := os.Stat("qwatrix.db"); err == nil {
		t.Errorf("the server wrote qwatrix.db into the test's own directory; AC-7 requires it to create nothing outside its data path")
	}

	elapsed, err := server.stop(syscall.SIGTERM, 15*time.Second)
	if err != nil {
		t.Fatalf("stopping cleanly: %v\nSTDERR:\n%s", err, server.output())
	}
	if elapsed != 0 {
		t.Errorf("stopping took %s, which means the shutdown ran past its grace period", elapsed)
	}
	if code := exitCode(t, server.cmd.ProcessState); code != 0 {
		t.Errorf("exited with %d, want 0 (a clean stop must not look like a failure to a supervisor)\nSTDERR:\n%s",
			code, server.output())
	}
}

// TestBootingCreatesNothingOutsideItsDataDirectory is the half of AC-7 that a test which only checks
// the data directory cannot make: the working directory must stay as it was.
func TestBootingCreatesNothingOutsideItsDataDirectory(t *testing.T) {
	workDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("making the data directory: %v", err)
	}
	address := freeLoopback(t)
	server := start(t, workDir, address, "-addr", address, "-data-dir", dataDir, "-server-name", "runtime.test")

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("reading the working directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the server created %d entries in its working directory (%v) while configured to keep its state in %s",
			len(entries), names(entries), dataDir)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "qwatrix.db")); err != nil {
		t.Errorf("the database is not in the configured data directory: %v\nthe data directory holds: %v",
			err, listing(t, dataDir))
	}
	if _, err := server.stop(syscall.SIGTERM, 15*time.Second); err != nil {
		t.Fatalf("stopping cleanly: %v\nSTDERR:\n%s", err, server.output())
	}
}

// TestSIGTERMLetsAnInFlightRequestFinish is AC-7's graceful-shutdown claim with the only proof that
// matters: a request that was already being served has to complete, not have its connection dropped.
//
// Without this, a rolling restart loses whatever was mid-flight, and a long-poll /sync is exactly where
// a Matrix client spends most of its life.
func TestSIGTERMLetsAnInFlightRequestFinish(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signalName(signal), func(t *testing.T) {
			dir := t.TempDir()
			address := freeLoopback(t)
			server := start(t, dir, address, "-addr", address, "-server-name", "runtime.test")
			token, _ := register(t, server.baseURL)

			// A two-second long-poll: long enough to still be in flight when the signal is sent,
			// short enough that the graceful window should comfortably finish it.
			inFlight := make(chan inFlightResult, 1)
			go func() {
				request, err := http.NewRequest(http.MethodGet,
					server.baseURL+"/_matrix/client/v3/sync?timeout=2000&access_token="+token, nil)
				if err != nil {
					inFlight <- inFlightResult{err: err}
					return
				}
				client := &http.Client{Timeout: 20 * time.Second}
				response, err := client.Do(request)
				if err != nil {
					inFlight <- inFlightResult{err: err}
					return
				}
				defer func() { _ = response.Body.Close() }()
				payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				inFlight <- inFlightResult{status: response.StatusCode, body: string(payload)}
			}()

			time.Sleep(400 * time.Millisecond)
			if _, err := server.stop(signal, 15*time.Second); err != nil {
				t.Fatalf("stopping with %s: %v\nSTDERR:\n%s", signalName(signal), err, server.output())
			}
			result := <-inFlight
			if result.err != nil {
				t.Fatalf("the in-flight request did not survive %s: %v\n"+
					"A graceful shutdown has to finish requests it already accepted.\nSTDERR:\n%s",
					signalName(signal), result.err, server.output())
			}
			if result.status != http.StatusOK {
				t.Errorf("the in-flight request answered %d, want 200: %s", result.status, result.body)
			}
			var sync struct {
				NextBatch string `json:"next_batch"`
			}
			if err := json.Unmarshal([]byte(result.body), &sync); err != nil || sync.NextBatch == "" {
				t.Errorf("the in-flight request answered with something that is not a sync response (err %v): %s",
					err, result.body)
			}
			if code := exitCode(t, server.cmd.ProcessState); code != 0 {
				t.Errorf("exited with %d after %s, want 0", code, signalName(signal))
			}
		})
	}
}

// inFlightResult carries what the client saw.
type inFlightResult struct {
	status int
	body   string
	err    error
}

// TestBootLogsCarryNoSecrets covers AC-7's log claim from the only vantage point a real operator has:
// the process's own output, from the first line of startup.
//
// The in-process equivalent cannot check this properly, because the harness captures logs into a buffer
// at error level, and an operator runs a server where startup is logged.
func TestBootLogsCarryNoSecrets(t *testing.T) {
	dir := t.TempDir()
	address := freeLoopback(t)
	const sharedSecret = "shared-secret-never-log-me"
	server := start(t, dir, address,
		"-addr", address, "-server-name", "runtime.test", "-registration-secret", sharedSecret)

	token, password := register(t, server.baseURL)
	if _, err := server.stop(syscall.SIGTERM, 15*time.Second); err != nil {
		t.Fatalf("stopping cleanly: %v\nSTDERR:\n%s", err, server.output())
	}

	logged := server.output()
	for _, secret := range []struct {
		name, value string
	}{
		{"the shared registration secret", sharedSecret},
		{"the user's password", password},
		{"the access token", token},
	} {
		if strings.Contains(logged, secret.value) {
			t.Errorf("%s appears in the server's own output; a log file is shipped to aggregators and pasted into channels\n"+
				"output:\n%s", secret.name, logged)
		}
	}
}

// exitCode digs the process exit status out of a completed command, failing the test if the process
// never reached an exit status at all.
func exitCode(t *testing.T, state *os.ProcessState) int {
	t.Helper()
	if state == nil {
		t.Fatalf("the process has not exited, so there is no exit code to check")
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			// A process that died of a signal did not exit 0, which is the whole claim being tested.
			t.Fatalf("the process was killed by signal %s rather than exiting on its own", status.Signal())
		}
		return status.ExitStatus()
	}
	return state.ExitCode()
}

func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGINT:
		return "SIGINT"
	default:
		return signal.String()
	}
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"<unreadable: " + err.Error() + ">"}
	}
	return names(entries)
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}
