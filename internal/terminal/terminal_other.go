//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package terminal

import "os"

// IsTerminal reports whether f is a terminal.
//
// The approximation, on a platform this package has no ioctl for. Windows in
// particular needs GetConsoleMode rather than an ioctl, and until somebody
// ships a Windows build worth testing, guessing generously beats refusing to
// prompt on a console that is really there.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}

	return charDevice(f)
}
