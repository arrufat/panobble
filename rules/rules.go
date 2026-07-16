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

//go:embed separators.json
var Separators []byte

//go:embed placeholders.json
var Placeholders []byte

//go:embed youtube_precleaners.json
var YoutubePrecleaners []byte

//go:embed youtube_extractors.json
var YoutubeExtractors []byte

//go:embed youtube_filters.json
var YoutubeFilters []byte

//go:embed presets.json
var Presets []byte
