package terminal

import (
	"os"
	"testing"
)

// The case the char-device approximation gets wrong, and the whole reason this
// package exists: /dev/null is a character device and is not a terminal.
func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s here: %v", os.DevNull, err)
	}
	defer f.Close()

	if IsTerminal(f) {
		t.Error("IsTerminal(/dev/null) = true, want false")
	}

	// And the approximation that was in use says the opposite, which is the
	// bug this replaces. Asserted so the test explains itself if it ever fails
	// the other way round.
	if !charDevice(f) {
		t.Skip("this platform does not report /dev/null as a character device, so there is nothing to contrast")
	}
}

func TestAPipeIsNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	if IsTerminal(r) {
		t.Error("IsTerminal(pipe) = true, want false")
	}
}

func TestAFileIsNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "term")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if IsTerminal(f) {
		t.Error("IsTerminal(regular file) = true, want false")
	}
}

func TestNilIsNotATerminal(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("IsTerminal(nil) = true, want false")
	}
}
