package output

import (
	"strings"
	"testing"
)

func TestCleanKeepsWhatALogIs(t *testing.T) {
	for _, s := range []string{
		"",
		"Installing dependencies\n",
		"col1\tcol2\r\n",
		"progress 10%\rprogress 100%\n",
		"\x1b[31merror\x1b[0m: missing script\n",
		"\x1b[1;32m✓\x1b[0m done — 2 ünïcode lines\n",
	} {
		if got := Clean(s); got != s {
			t.Errorf("Clean(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestCleanRemovesWhatATerminalWouldAct(t *testing.T) {
	for in, want := range map[string]string{
		// The cursor moved up and the line rewritten: a failure shown as success.
		"FAILED\x1b[1A\x1b[2KSucceeded\n": "FAILEDSucceeded\n",
		// The screen cleared.
		"before\x1b[2J\x1b[Hafter": "beforeafter",
		// The window title set, ended by BEL and by ST.
		"\x1b]0;owned\x07text":   "text",
		"\x1b]2;owned\x1b\\text": "text",
		// The clipboard written (OSC 52).
		"\x1b]52;c;cm0gLXJmIH4=\x07ok": "ok",
		// A hyperlink, whose target is not what it shows.
		"\x1b]8;;https://evil.test\x1b\\click\x1b]8;;\x1b\\": "click",
		// The terminal asked to answer back.
		"\x1b[6n\x1b[c\x1bZx": "x",
		// A colour code with a command hidden in it is not colour.
		"\x1b[31;?1049hx": "x",
		// C0 and C1 controls: bell, backspace, a C1 CSI.
		"a\x07b\x08c\u009b2Jd\x7fe": "abc2Jde",
		// DCS.
		"\x1bP+q544e\x1b\\x": "x",
		// An escape cut off at the end of a chunk.
		"tail\x1b[": "tail",
		"tail\x1b":  "tail",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanMarksInvalidUTF8RatherThanPassingIt(t *testing.T) {
	if got := Clean("a\x9bb"); got != "a�b" {
		t.Errorf("a raw 0x9b byte, a C1 CSI to some terminals, came through as %q", got)
	}
}

// What the printer writes is cleaned: a server's message, an error, a name
// in a table.
func TestPrinterCleansWhatItWrites(t *testing.T) {
	var out, errs strings.Builder
	p, err := NewPrinter(&out, &errs, string(FormatTable))
	if err != nil {
		t.Fatal(err)
	}

	p.Say("deploy of %s", "site\x1b]52;c;cm0=\x07")
	p.Warn("%s", "\x1b[2Jcleared")
	p.Line("%s", "url\x1b[1A")
	if err := p.Print(Table{Columns: []string{"name"}, Rows: [][]string{{"acme\x1b]0;owned\x07\tsite\nx"}}}, nil); err != nil {
		t.Fatal(err)
	}

	for _, written := range []string{out.String(), errs.String()} {
		for _, bad := range []string{"\x1b]", "\x1b[2J", "\x1b[1A", "\x07"} {
			if strings.Contains(written, bad) {
				t.Errorf("the printer wrote %q: %q", bad, written)
			}
		}
	}
	if !strings.Contains(out.String(), "acme site x") {
		t.Errorf("a cell's tab and newline were not flattened: %q", out.String())
	}
}
