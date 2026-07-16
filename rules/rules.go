// Package rules embeds panobble's language-neutral rule data.
//
// Every file in this directory is plain JSON with RE2-compatible regex
// patterns, so a port to another language only needs to reimplement the
// small interpreters that consume them, not the rules themselves.
// Each file documents the pano-scrobbler source it was extracted from.
package rules

import _ "embed"

//go:embed wildcard_domains.json
var WildcardDomains []byte
