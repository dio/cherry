// Package repl provides an embeddable diagnostic command executor for opened
// Cherry bundles.
//
// The package is intentionally a consumer-side layer over the root Cherry
// reader APIs. It does not build bundles, fetch bundles, authenticate
// operators, authorize scopes, or choose a transport. Embedding applications
// provide a Backend, optional runtime Context, and any terminal, HTTP, or RPC
// loop they want to expose.
//
// Session state has two separate concepts that should not be collapsed:
// Context.Lane is embedding metadata, such as an Orange snapshot lane, while
// Session.ActiveScope is the Cherry enforcement scope passed to Reader
// resolution and inspector APIs.
package repl
