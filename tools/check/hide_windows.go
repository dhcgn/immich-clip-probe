package check

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps the desktop app, a GUI process, from flashing a console
// window for every magick run.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
