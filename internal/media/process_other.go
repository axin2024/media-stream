//go:build !windows

package media

import "os/exec"

func hideConsole(_ *exec.Cmd) {}
