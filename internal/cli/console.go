package cli

// Where in the console a person does something the CLI cannot do for them.
//
// Both of these are pages, and neither is reachable at the address this used
// to print. The console's account pages hang on Drupal's own `/user/{user}` —
// `/user/7/tokens`, `/user/7/ssh-keys` — and `/account/tokens` was never a
// route at all, so six messages sent people to a 404. Nothing caught it: the
// strings are formatted from configuration, so they are well-formed URLs on
// both sides of a build, and no test on either side asks a router whether the
// path exists.
//
// A uid-less address is the only one that can be printed. The id is not always
// known — at the paste-a-token prompt there is no credential yet, so there is
// nobody to be the `{user}` — and a helper that needed it would work in four
// places and not in the two that matter most. `/account` is the console's own
// redirect to whoever is signed in, put there for exactly this: "where old
// links land". So the URL names the account and the sentence names the tab.
const (
	// consoleAccount redirects to the signed-in person's own pages.
	consoleAccount = "/account"

	// The tabs, spelled as the console spells them, because somebody is about
	// to look for these words on a page.
	tokensTab = "Access tokens"
	keysTab   = "SSH keys"
)

// tokensPage locates where a personal access token is minted and revoked.
func tokensPage(api string) string {
	return api + consoleAccount + ", under " + tokensTab
}

// keysPage locates where an SSH key is added.
func keysPage(api string) string {
	return api + consoleAccount + ", under " + keysTab
}
