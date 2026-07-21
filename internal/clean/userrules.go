package clean

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/scrobble"
)

var ruleFields = []string{"track", "album", "artist", "albumArtist"}

// userRule is a compiled config.Rule.
type userRule struct {
	name             string
	search           map[string]*regexp.Regexp // field -> pattern (non-empty only)
	replacement      map[string]string
	apps             []string
	hosts            []string
	replaceAll       bool
	block            bool
	continueMatching bool
}

func compileUserRules(rules []config.Rule) ([]userRule, error) {
	var out []userRule
	for _, r := range rules {
		ur := userRule{
			name:             r.Name,
			search:           make(map[string]*regexp.Regexp),
			replacement:      r.Replacement,
			apps:             r.Apps,
			hosts:            r.Hosts,
			replaceAll:       r.ReplaceAll,
			block:            r.Block,
			continueMatching: r.ContinueMatching,
		}
		for field, pattern := range r.Search {
			if pattern == "" {
				continue
			}
			if !slices.Contains(ruleFields, field) {
				return nil, fmt.Errorf("rule %q: unknown field %q", r.Name, field)
			}
			p := pattern
			if !r.CaseSensitive {
				p = "(?i)" + p
			}
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("rule %q, field %s: %w", r.Name, field, err)
			}
			ur.search[field] = re
		}
		if len(ur.search) == 0 {
			return nil, fmt.Errorf("rule %q has no search patterns", r.Name)
		}
		out = append(out, ur)
	}
	return out, nil
}

// applyUserRules applies the configured rules in order. Returns the
// possibly-changed track, whether any rule matched, the name of a blocking
// rule ("" if none), and whether a matched rule had continue_matching =
// false (which also stops presets from running).
func (p *Pipeline) applyUserRules(t scrobble.Track, host string) (scrobble.Track, bool, string, bool) {
	changed := false
	stopped := false

	for i := range p.userRules {
		rule := &p.userRules[i]
		if len(rule.apps) > 0 && !slices.Contains(rule.apps, t.AppID) {
			continue
		}
		if len(rule.hosts) > 0 && !slices.Contains(rule.hosts, host) {
			continue
		}

		// All non-empty patterns must match their field.
		allMatched := true
		for field, re := range rule.search {
			if !re.MatchString(*fieldValue(&t, field)) {
				allMatched = false
				break
			}
		}
		if !allMatched {
			continue
		}

		if rule.block {
			return t, changed, ruleLabel(rule), true
		}

		nt := t
		for field, re := range rule.search {
			val := fieldValue(&nt, field)
			repl := rule.replacement[field]
			if rule.replaceAll {
				*val = re.ReplaceAllString(*val, repl)
			} else {
				*val = replaceFirst(re, *val, repl)
			}
		}
		// A replacement that empties track or artist is discarded.
		if nt.Title != "" && nt.Artist != "" {
			t = nt
			changed = true
		}

		if changed && !rule.continueMatching {
			stopped = true
			break
		}
	}

	return t, changed, "", stopped
}

func ruleLabel(r *userRule) string {
	if r.name != "" {
		return r.name
	}
	return "unnamed rule"
}
