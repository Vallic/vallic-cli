package cli

import (
	"fmt"
	"time"

	"github.com/vallic/vallic-cli/internal/auth"
)

// How close a deadline has to be before the CLI starts saying so.
//
// Fourteen days because renewing means a person going to the console and a
// pipeline's secret being replaced, which is somebody's working week rather
// than a command. Warning earlier would be noise for the eleven weeks of a
// ninety-day token where nothing is required; warning later would put the
// first notice inside the window where a holiday covers the whole of it.
const expiryWarning = 14 * 24 * time.Hour

// And the point at which saying it once a day is not enough.
const expiryUrgent = 3 * 24 * time.Hour

// warnAboutExpiry says a credential is running out, where one is.
//
// On stderr, like everything the CLI says about itself: `vallic url` and
// `vallic var get` exist to be captured, and a deadline notice on stdout
// would end up inside a shell variable.
//
// Printed after the command rather than before it. Whether a credential was
// needed at all is not known until the command has run — `vallic init` and
// `vallic validate --local` reach nothing — and forcing a credential to be
// loaded just to check its deadline would make those two fail for want of
// one.
func warnAboutExpiry(e *Env, now time.Time) {
	if e.creds == nil {
		// Nothing was loaded, so nothing was used.
		return
	}

	left, known := e.creds.Remaining(now)
	if !known {
		// An older control plane, or an OAuth session, which has no deadline
		// of this kind. Saying nothing is right: a warning about a date
		// nobody stated would be the CLI guessing.
		return
	}

	if left > expiryWarning {
		return
	}

	if left <= 0 {
		// Past it. Said even though the next request will fail anyway,
		// because "unauthorized" on its own sends somebody to check their
		// permissions rather than their calendar.
		e.Printer.Warn("This credential ran out %s.", humanAgo(-left))
		e.Printer.Say("  %s", renewAdvice(e.creds))

		return
	}

	e.Printer.Warn("This credential runs out %s.", humanIn(left))
	e.Printer.Say("  %s", renewAdvice(e.creds))

	if left <= expiryUrgent {
		// The sentence that makes it somebody's problem today. A pipeline
		// that stops on a Saturday is found on a Monday.
		e.Printer.Say("  nothing renews it, so whatever uses it stops working then")
	}
}

// renewAdvice says what to do about it, which differs by credential.
func renewAdvice(creds *auth.Credentials) string {
	if creds.Kind == auth.KindOAuth {
		return "sign in again: vallic login"
	}

	// Not "rotate": there is deliberately no way for a token to mint its
	// successor, because a credential that can always replace itself makes
	// its own lifetime a liveness check rather than a limit. A person mints
	// the next one.
	return fmt.Sprintf("mint a new one at %s, then: vallic login --token", tokensPage(creds.API))
}

// humanIn renders a duration as the deadline somebody plans around.
//
// Days, because that is the unit a renewal is scheduled in, and rounded down
// so "2 days" never means ten minutes from now. Hours below a day for the
// same reason in the other direction: "0 days" reads as already gone.
func humanIn(left time.Duration) string {
	switch days := int(left / (24 * time.Hour)); {
	case days >= 2:
		return fmt.Sprintf("in %d days", days)
	case days == 1:
		return "tomorrow"
	case left >= time.Hour:
		return fmt.Sprintf("in %d hours", int(left/time.Hour))
	default:
		return "within the hour"
	}
}

// humanAgo renders how long ago a deadline passed.
func humanAgo(since time.Duration) string {
	switch days := int(since / (24 * time.Hour)); {
	case days >= 2:
		return fmt.Sprintf("%d days ago", days)
	case days == 1:
		return "yesterday"
	case since >= time.Hour:
		return fmt.Sprintf("%d hours ago", int(since/time.Hour))
	default:
		return "within the last hour"
	}
}
