//go:build linux

package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}

	var termios syscall.Termios

	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TCGETS,
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)

	return errno == 0
}
