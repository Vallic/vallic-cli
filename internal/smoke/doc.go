// Package smoke drives the built binary against a real control plane.
//
// Everything else in this repository tests the CLI against itself: unit tests
// against hand-written payloads, and for a while a stand-in control plane
// written from the same specification as the client. That catches a great deal
// and it could not, even in principle, catch the three worst bugs this CLI has
// had. Each was a disagreement between what the client assumed and what the
// platform actually sends, and both halves of every one of those tests were
// written from the same assumption.
//
//   - `vallic deploy --release 7` sent the project's release *number* where the
//     route loads by entity *id*. It refused outright on real data. On a young
//     install where ids and numbers still coincide it would have worked in
//     testing and silently deployed the wrong build later.
//   - `validate` could not decode its own response. PHP has one array type for
//     lists and maps, so an empty `services[].environment` arrives as `[]`, and
//     unmarshalling that into a Go map fails the whole response rather than the
//     field.
//   - `status` printed `release 3339` for what the project calls release 7,
//     which is a number somebody would then type into `--release`.
//
// gofmt, vet, go test, phpcs and phpstan were all green through every one of
// them. What found them was minting a token and running the binary. That is
// what this package automates.
//
// # It is read-only, and that is load-bearing
//
// Every command here reads. None deploys, restores, takes a backup, claims a
// hostname, writes a variable or creates an environment. This is not caution
// for its own sake: a `--release` test against a control plane with a live
// agent queued a real deployment and the agent ran it. A smoke test that can
// change a customer's site is a smoke test nobody dares point at anything
// worth testing against.
//
// # Running it
//
//	VALLIC_API=https://vallic.ddev.site \
//	VALLIC_TOKEN=vcp_... \
//	go test -tags smoke ./internal/smoke/
//
// Without both variables every test skips, so `go test ./...` on a laptop or
// in a fork's CI stays green and says why.
//
// Build-tagged so the ordinary suite cannot accidentally depend on a network,
// a credential or somebody else's data.
package smoke
