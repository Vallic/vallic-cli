package cli

import (
	"regexp"
	"strings"
	"testing"
)

// The console has no page under `/account`, and printing one sent people to a
// 404 from six places for as long as the messages existed.
//
// `/account` is a redirect to the signed-in person's own pages and takes no
// path of its own; the real pages are `/user/{user}/tokens` and
// `/user/{user}/ssh-keys`, which the CLI cannot address because it does not
// always know who `{user}` is. So the rule this asserts is the narrow one that
// was actually broken: whatever these produce, the path stops at `/account`.
func TestTheConsoleLocatorsNameNoPageUnderAccount(t *testing.T) {
	// The URL, up to the first character that ends one in an English sentence.
	url := regexp.MustCompile(`https://console\.example[^\s,]*`)

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"tokens", tokensPage("https://console.example")},
		{"keys", keysPage("https://console.example")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := url.FindString(tc.got)
			if found == "" {
				t.Fatalf("%s = %q, want it to contain a URL", tc.name, tc.got)
			}

			if found != "https://console.example/account" {
				t.Errorf("URL = %q, want it to stop at /account: no route exists below it", found)
			}
		})
	}
}

// The URL cannot say which person, so the sentence has to say which tab --
// otherwise `/account` lands somebody on an overview with no idea what to
// click. Spelled as the console spells it, because they are about to look for
// these words on a page.
func TestTheConsoleLocatorsNameTheirTab(t *testing.T) {
	if !strings.Contains(tokensPage("https://x"), "Access tokens") {
		t.Error("the tokens locator does not name its tab")
	}

	if !strings.Contains(keysPage("https://x"), "SSH keys") {
		t.Error("the keys locator does not name its tab")
	}
}
