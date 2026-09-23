//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testCLIMode = "GH_ROUTER_TEST_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(testCLIMode) == "1" {
		if err := run(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "gh-router: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExecCommandLifecycleAndEnvironment(t *testing.T) {
	configPath := writeExecTestConfig(t)
	command := exec.Command(os.Args[0],
		"exec", "-config", configPath, "-hint", "related", "--",
		"/bin/sh", "-c", `
printf '%s\n' "$$"
printf '%s\n' "$HTTPS_PROXY"
printf '%s\n' "$HTTP_PROXY"
printf '%s\n' "$SSL_CERT_FILE"
printf '%s\n' "$GH_HOST"
printf '%s\n' "$GH_TOKEN"
printf '%s\n' "${TEST_CLIENT_TOKEN-unset}"
printf '%s\n' "${TEST_MAIN_TOKEN-unset}"
printf '%s\n' "${TOKEN_ALIAS-unset}"
read ignored || true
`,
	)
	command.Env = execTestEnvironment()
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(stdout)
	lines := make([]string, 9)
	for i := range lines {
		if !scanner.Scan() {
			stdin.Close()
			_ = command.Wait()
			t.Fatalf("read command output line %d: %v\nstderr: %s", i, scanner.Err(), stderr.String())
		}
		lines[i] = scanner.Text()
	}

	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid != command.Process.Pid {
		t.Fatalf("command PID = %q, want executor PID %d", lines[0], command.Process.Pid)
	}
	proxyURL := lines[1]
	if !strings.HasPrefix(proxyURL, "http://127.0.0.1:") || lines[2] != proxyURL {
		t.Fatalf("proxy environment = HTTPS_PROXY %q, HTTP_PROXY %q", proxyURL, lines[2])
	}
	caPath := lines[3]
	if lines[4] != "github.com" || lines[5] != "client-secret:related" {
		t.Fatalf("GitHub environment = GH_HOST %q, GH_TOKEN %q", lines[4], lines[5])
	}
	for i, value := range lines[6:] {
		if value != "unset" {
			t.Fatalf("secret environment line %d = %q, want unset", i+6, value)
		}
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read temporary CA certificate: %v", err)
	}
	if !bytes.Contains(caPEM, []byte("BEGIN CERTIFICATE")) || bytes.Contains(caPEM, []byte("PRIVATE KEY")) {
		t.Fatalf("temporary CA file contains unexpected PEM blocks:\n%s", caPEM)
	}
	info, err := os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary CA mode = %o, want 600", info.Mode().Perm())
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   time.Second,
	}
	response, err := client.Get(proxyURL + "/ca.pem")
	if err != nil {
		t.Fatalf("get sidecar CA certificate: %v", err)
	}
	servedCA, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Equal(servedCA, caPEM) {
		t.Fatalf("sidecar CA response = %d, equal file = %v", response.StatusCode, bytes.Equal(servedCA, caPEM))
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("exec command failed: %v\nstderr: %s", err, stderr.String())
	}
	for _, secret := range []string{"main-secret", "client-secret"} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("stderr contains secret %q: %s", secret, stderr.String())
		}
	}
	waitForSidecarCleanup(t, client, proxyURL, caPath)
}

func TestExecCommandPreservesExitStatus(t *testing.T) {
	command := exec.Command(os.Args[0], "exec", "-config", writeExecTestConfig(t), "--", "/bin/sh", "-c", "exit 23")
	command.Env = execTestEnvironment()
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("exec command error = %v, want exit code 23", err)
	}
}

func TestExecCommandPreservesTerminationSignal(t *testing.T) {
	command := exec.Command(os.Args[0], "exec", "-config", writeExecTestConfig(t), "--", "/bin/sh", "-c", "printf 'ready\\n'; exec /bin/sleep 30")
	command.Env = execTestEnvironment()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || line != "ready\n" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("wait for command readiness = %q, %v", line, err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("exec command error = %v, want signal exit", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("command wait status = %#v, want SIGTERM", exitErr.Sys())
	}
}

func TestRunExecRequiresCommand(t *testing.T) {
	if err := runExec(nil); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("runExec(nil) error = %v", err)
	}
}

func writeExecTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "version": 1,
  "server": {"listen": "0.0.0.0:1"},
  "authentication": {"tokenEnv": "TEST_CLIENT_TOKEN"},
  "routing": {"routes": [{"credential": "main"}]},
  "credentials": [
    {"id": "main", "tokenEnv": "TEST_MAIN_TOKEN", "hints": ["related"]}
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func execTestEnvironment() []string {
	removed := map[string]bool{
		testCLIMode:         true,
		"TEST_CLIENT_TOKEN": true,
		"TEST_MAIN_TOKEN":   true,
		"TOKEN_ALIAS":       true,
		"GH_TOKEN":          true,
		"GITHUB_TOKEN":      true,
	}
	environment := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !removed[name] {
			environment = append(environment, entry)
		}
	}
	return append(environment,
		testCLIMode+"=1",
		"TEST_CLIENT_TOKEN=client-secret",
		"TEST_MAIN_TOKEN=main-secret",
		"TOKEN_ALIAS=main-secret",
		"GH_TOKEN=unknown-client-token",
		"GITHUB_TOKEN=unknown-github-token",
	)
}

func waitForSidecarCleanup(t *testing.T, client *http.Client, proxyURL, caPath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, statErr := os.Stat(caPath)
		response, requestErr := client.Get(proxyURL + "/ca.pem")
		if response != nil {
			response.Body.Close()
		}
		if errors.Is(statErr, os.ErrNotExist) && requestErr != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("sidecar resources were not cleaned up: CA %q, proxy %q", caPath, proxyURL)
}
