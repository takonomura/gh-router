//go:build !unix

package ghrouter

import "os/exec"

func configureTokenCommand(cmd *exec.Cmd) {}
