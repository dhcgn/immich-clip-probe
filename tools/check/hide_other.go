//go:build !windows

package check

import "os/exec"

func hideWindow(*exec.Cmd) {}
