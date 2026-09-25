//go:build unix

package ghrouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const (
	sidecarStartupTimeout = 10 * time.Second
	sidecarStopTimeout    = 2 * time.Second
)

var lookPath = exec.LookPath

func ExecCommand(configPath, hint string, command []string) error {
	if len(command) == 0 {
		return errors.New("exec: command is required")
	}
	commandPath, err := executablePath(command[0])
	if err != nil {
		return fmt.Errorf("exec: find command %q: %w", command[0], err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	credential, err := clientCredential(cfg, hint)
	if err != nil {
		return err
	}

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("exec: create readiness pipe: %w", err)
	}
	defer readyReader.Close()

	authenticationReader, authenticationWriter, err := os.Pipe()
	if err != nil {
		readyWriter.Close()
		return fmt.Errorf("exec: create authentication pipe: %w", err)
	}
	defer authenticationReader.Close()
	defer authenticationWriter.Close()

	self, err := os.Executable()
	if err != nil {
		readyWriter.Close()
		return fmt.Errorf("exec: locate gh-router executable: %w", err)
	}
	sidecar := exec.Command(self,
		SidecarCommand,
		"-config", configPath,
		"-parent-pid", strconv.Itoa(os.Getpid()),
	)
	sidecar.Env = os.Environ()
	sidecar.ExtraFiles = []*os.File{readyWriter, authenticationReader}
	sidecar.Stderr = os.Stderr
	sidecar.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sidecar.Start(); err != nil {
		readyWriter.Close()
		return fmt.Errorf("exec: start sidecar: %w", err)
	}
	readyWriter.Close()
	authenticationReader.Close()
	// The readiness deadline also bounds a stalled transfer to the sidecar.
	transferred := make(chan error, 1)
	go func() {
		_, err := io.WriteString(authenticationWriter, cfg.Authentication.token)
		authenticationWriter.Close()
		transferred <- err
	}()

	ready, err := waitForSidecarReady(readyReader)
	if err != nil {
		stopSidecar(sidecar)
		return err
	}
	if err := validateReady(ready); err != nil {
		stopSidecar(sidecar)
		return fmt.Errorf("exec: sidecar startup: %w", err)
	}
	if err := <-transferred; err != nil {
		stopSidecar(sidecar)
		return errors.New("exec: transfer proxy authentication failed")
	}
	if err := readyReader.Close(); err != nil {
		stopSidecar(sidecar)
		return fmt.Errorf("exec: close readiness pipe: %w", err)
	}

	environment := commandEnvironment(os.Environ(), cfg, credential, ready.ProxyURL, ready.CACertificate)
	argv := append([]string(nil), command...)
	if err := syscall.Exec(commandPath, argv, environment); err != nil {
		stopSidecar(sidecar)
		return fmt.Errorf("exec %q: %w", command[0], err)
	}
	return nil
}

func waitForSidecarReady(reader io.Reader) (sidecarReady, error) {
	type result struct {
		ready sidecarReady
		err   error
	}
	results := make(chan result, 1)
	go func() {
		var ready sidecarReady
		err := json.NewDecoder(io.LimitReader(reader, 64<<10)).Decode(&ready)
		results <- result{ready: ready, err: err}
	}()

	timer := time.NewTimer(sidecarStartupTimeout)
	defer timer.Stop()
	select {
	case result := <-results:
		if result.err != nil {
			return sidecarReady{}, fmt.Errorf("exec: read sidecar readiness: %w", result.err)
		}
		return result.ready, nil
	case <-timer.C:
		return sidecarReady{}, errors.New("exec: sidecar startup timed out")
	}
}

func stopSidecar(sidecar *exec.Cmd) {
	if sidecar.Process == nil {
		return
	}
	_ = sidecar.Process.Signal(syscall.SIGTERM)
	waited := make(chan struct{})
	go func() {
		_ = sidecar.Wait()
		close(waited)
	}()
	timer := time.NewTimer(sidecarStopTimeout)
	defer timer.Stop()
	select {
	case <-waited:
		return
	case <-timer.C:
	}
	_ = sidecar.Process.Kill()
	<-waited
}
