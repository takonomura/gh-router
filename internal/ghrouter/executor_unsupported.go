//go:build !unix

package ghrouter

import (
	"errors"
	"os"
	"os/exec"
)

var lookPath = exec.LookPath

func ExecCommand(configPath, hint string, command []string) error {
	return errors.New("gh-router exec is supported only on Unix-like systems")
}

func RunSidecar(configPath string, parentPID int, ready, authentication *os.File) error {
	return errors.New("gh-router exec is supported only on Unix-like systems")
}
