// Package clean is the metadata cleanup pipeline: sanitize, regex presets,
// YouTube title parsing, and user regex edits.
//
// Ported from pano-scrobbler: MetadataUtils.kt (YouTube parser, splitter,
// sanitize), RegexPresets.kt (presets, 🅴 trim, "- Topic"), RegexEditsDao.kt
// (user rules), and ScrobbleEverywhere.preprocessMetadata (pipeline order).
package clean

import (
	"encoding/json"
	"regexp"

	"github.com/arrufat/panobble/rules"
)

// filterRule mirrors the JSON rule shape shared by precleaners and filters.
type filterRule struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
	CI          bool   `json:"ci"`
	Global      bool   `json:"global"`
}

type compiledRule struct {
	re          *regexp.Regexp
	replacement string
	global      bool
}

func (r filterRule) compile() compiledRule {
	p := r.Pattern
	if r.CI {
		p = "(?i)" + p
	}
	return compiledRule{re: regexp.MustCompile(p), replacement: r.Replacement, global: r.Global}
}

// apply runs the rule: replace-all when global, replace-first otherwise.
func (c compiledRule) apply(s string) string {
	if c.global {
		return c.re.ReplaceAllString(s, c.replacement)
	}
	return replaceFirst(c.re, s, c.replacement)
}

// replaceFirst replaces the first match with group expansion (RE2 has no
// built-in ReplaceFirst).
func replaceFirst(re *regexp.Regexp, s, replacement string) string {
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	expanded := re.ExpandString(nil, replacement, s, loc)
	return s[:loc[0]] + string(expanded) + s[loc[1]:]
}

type extractor struct {
	Pattern     string `json:"pattern"`
	ArtistGroup int    `json:"artist_group"`
	TrackGroup  int    `json:"track_group"`
}

type compiledExtractor struct {
	re                      *regexp.Regexp
	artistGroup, trackGroup int
}

func (e extractor) compile() compiledExtractor {
	return compiledExtractor{
		re:          regexp.MustCompile(e.Pattern),
		artistGroup: e.ArtistGroup,
		trackGroup:  e.TrackGroup,
	}
}

type preset struct {
	Name        string   `json:"name"`
	Fields      []string `json:"fields"`
	Pattern     string   `json:"pattern"`
	Replacement string   `json:"replacement"`
	Hosts       []string `json:"hosts"`
	EdgeSymbol  string   `json:"edge_symbol"`
}

type compiledPreset struct {
	name        string
	fields      []string
	re          *regexp.Regexp
	replacement string
	hosts       []string
	edgeSymbol  string
}

// Loaded rule data, compiled once at package init. MustCompile panics on any
// non-RE2 pattern, so a bad rules file fails loudly at startup (and in every
// test run).
var (
	precleaners    []compiledRule
	extractorsPre  []compiledExtractor
	extractorsPost []compiledExtractor
	trackFilters   []compiledRule
	artistFilters  []compiledRule
	regexPresets   []compiledPreset
	separators     []string
	metaUnknown    map[string]bool
	artistSpam     map[string]bool
)

func init() {
	// precleaners
	var pc struct {
		Rules []filterRule `json:"rules"`
	}
	mustUnmarshal(rules.YoutubePrecleaners, &pc)
	for _, r := range pc.Rules {
		r.Global = true // precleaners always replace all occurrences
		precleaners = append(precleaners, r.compile())
	}

	// extractors
	var ex struct {
		BeforeSplit []extractor `json:"before_split"`
		AfterSplit  []extractor `json:"after_split"`
	}
	mustUnmarshal(rules.YoutubeExtractors, &ex)
	for _, e := range ex.BeforeSplit {
		extractorsPre = append(extractorsPre, e.compile())
	}
	for _, e := range ex.AfterSplit {
		extractorsPost = append(extractorsPost, e.compile())
	}

	// filter stages
	var yf struct {
		Stages       map[string][]filterRule `json:"stages"`
		TrackStages  []string                `json:"track_stages"`
		ArtistStages []string                `json:"artist_stages"`
	}
	mustUnmarshal(rules.YoutubeFilters, &yf)
	for _, stage := range yf.TrackStages {
		for _, r := range yf.Stages[stage] {
			trackFilters = append(trackFilters, r.compile())
		}
	}
	for _, stage := range yf.ArtistStages {
		for _, r := range yf.Stages[stage] {
			artistFilters = append(artistFilters, r.compile())
		}
	}

	// presets
	var pr struct {
		Presets []preset `json:"presets"`
	}
	mustUnmarshal(rules.Presets, &pr)
	for _, p := range pr.Presets {
		regexPresets = append(regexPresets, compiledPreset{
			name:        p.Name,
			fields:      p.Fields,
			re:          regexp.MustCompile("(?i)" + p.Pattern), // RegexEdit default: case-insensitive
			replacement: p.Replacement,
			hosts:       p.Hosts,
			edgeSymbol:  p.EdgeSymbol,
		})
	}

	// separators + placeholders
	mustUnmarshal(rules.Separators, &separators)
	var ph struct {
		MetaUnknown []string `json:"meta_unknown"`
		ArtistSpam  []string `json:"artist_spam"`
	}
	mustUnmarshal(rules.Placeholders, &ph)
	metaUnknown = toSet(ph.MetaUnknown)
	artistSpam = toSet(ph.ArtistSpam)
}

func mustUnmarshal(data []byte, v any) {
	if err := json.Unmarshal(data, v); err != nil {
		panic("clean: bad rules JSON: " + err.Error())
	}
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
