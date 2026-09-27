//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is a terminal.
//
// The same question as on Linux under a different name: the BSDs spell the
// terminal-attributes ioctl TIOCGETA.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}

	var termios syscall.Termios

	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TIOCGETA,
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)

	return errno == 0
}
