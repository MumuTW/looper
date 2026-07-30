// Package glossary holds no runtime code. It exists to make CONTEXT.md's
// references to code checkable by `go test ./...`.
//
// CONTEXT.md used to restate concepts that code also defined, which meant two
// copies and no mechanism to notice when they diverged — the file has been
// touched 11 times against internal/'s 477, and its own named "semantic
// Authority", Triage Report, had no corresponding identifier at all. Entries
// are being converted to point at the type or package that defines the
// concept, so that there is one definition rather than two.
//
// That conversion trades one failure mode for another. A restated definition
// goes quietly stale; a pointer goes quietly wrong, which is worse, because a
// reader following `forge.ReviewItemID` to nothing has been actively misled.
// The tests here close that: a pointer that stops resolving fails the build,
// so a rename has to update the glossary in the same commit.
package glossary
