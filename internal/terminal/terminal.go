// Package terminal answers whether a file is a terminal somebody is looking at.
//
// Its own package because the answer decides two different things — whether to
// prompt and whether to colour — and both were asking it separately with the
// same wrong approximation.
//
// That approximation was `Mode()&os.ModeCharDevice != 0`, which is the usual
// one and is wrong for the case that matters: /dev/null is a character device.
// A pipeline that runs `vallic env delete staging` with stdin on /dev/null was
// told it had a terminal, printed a confirmation prompt, read end-of-file and
// reported "cancelled" — safe, and a much worse answer than "there is no
// terminal to confirm on, pass --yes".
//
// So this asks the kernel instead: a terminal is a file descriptor that
// answers the terminal-attributes ioctl. Nothing else does.
package terminal

import "os"

// charDevice is the fallback for platforms with no ioctl implementation here.
//
// Deliberately the old approximation rather than a hard false: reporting no
// terminal where there is one would turn every prompt into a refusal.
func charDevice(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
