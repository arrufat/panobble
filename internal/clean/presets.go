package clean

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Host constants (pano Stuff.kt).
const (
	hostYoutube      = "youtube.com"
	hostYoutubeMusic = "music.youtube.com"
)

const topicSuffix = "- Topic"

// applyPresets ports RegexPresets.applyAllPresets. Preset order is pano's
// enum order: parse_title, parse_title_with_fallback, then the regex presets.
// dataIsEdited skips the parse_title presets (pano's applyOncePresets rule).
// Returns the possibly-changed track, whether anything changed, and whether
// a strict title parse failed (⇒ the scrobble should be skipped).
func (p *Pipeline) applyPresets(t scrobble.Track, host string, dataIsEdited bool) (scrobble.Track, bool, bool) {
	changed := false

	// parse_title presets first (enum order).
	if !dataIsEdited {
		for _, name := range []string{"parse_title", "parse_title_with_fallback"} {
			if !p.presetEnabled[name] {
				continue
			}
			nt, ok, failed := p.applyParseTitle(t, host, name)
			if failed {
				return t, changed, true
			}
			if ok {
				t = nt
				changed = true
			}
		}
	}

	for _, preset := range regexPresets {
		if !p.presetEnabled[preset.name] {
			continue
		}
		if len(preset.hosts) > 0 && !contains(preset.hosts, host) {
			continue
		}
		if nt, ok := applyRegexPreset(t, preset); ok {
			t = nt
			changed = true
		}
	}

	return t, changed, false
}

// applyParseTitle ports the parse_title / parse_title_with_fallback branch of
// RegexPresets.applyPreset. The strict variant (parse_title, plain youtube.com)
// reports failure when the title cannot be parsed; pano throws
// TitleParseException there and the scrobble is skipped.
func (p *Pipeline) applyParseTitle(t scrobble.Track, host, presetName string) (out scrobble.Track, changed, parseFailed bool) {
	strict := presetName == "parse_title"

	applies := contains(p.parseTitleApps, t.AppID) ||
		(strict && host == hostYoutube) ||
		(!strict && host == hostYoutubeMusic)
	if !applies {
		return t, false, false
	}

	artistIsTopic := strings.HasSuffix(t.Artist, topicSuffix)
	shouldParse := t.Album == "" && !artistIsTopic

	switch {
	case shouldParse:
		artist, track := ParseYoutubeTitle(t.Title)
		if artist != "" && track != "" {
			t.Artist = artist
			t.Title = track
			t.Album = ""
			t.AlbumArtist = ""
			return t, true, false
		}
		if strict {
			return t, false, true
		}
		return t, false, false

	case artistIsTopic:
		t.Artist = strings.TrimSpace(strings.TrimSuffix(t.Artist, topicSuffix))
		t.AlbumArtist = strings.TrimSpace(strings.TrimSuffix(t.AlbumArtist, topicSuffix))
		return t, true, false
	}

	return t, false, false
}

// applyRegexPreset applies one regex preset (replace-first, per field), plus
// the explicit preset's 🅴 edge-symbol trim.
func applyRegexPreset(t scrobble.Track, preset compiledPreset) (scrobble.Track, bool) {
	changed := false
	for _, field := range preset.fields {
		val := fieldValue(&t, field)
		if *val == "" {
			continue
		}
		if preset.re.MatchString(*val) {
			*val = replaceFirst(preset.re, *val, preset.replacement)
			changed = true
		}
	}

	if preset.edgeSymbol != "" {
		if v, ok := removeEdgeSymbol(t.Title, preset.edgeSymbol); ok {
			t.Title = v
			changed = true
		}
		if t.Album != "" {
			if v, ok := removeEdgeSymbol(t.Album, preset.edgeSymbol); ok {
				t.Album = v
				changed = true
			}
		}
	}

	return t, changed
}

// removeEdgeSymbol ports RegexPresets.removeEdgeSymbol: removes symbol when
// it occurs exactly once, at the start (followed by whitespace) or at the
// end (preceded by whitespace).
func removeEdgeSymbol(input, symbol string) (string, bool) {
	first := strings.Index(input, symbol)
	last := strings.LastIndex(input, symbol)
	if first == -1 || first != last {
		return input, false
	}

	switch {
	case first == 0 && len(input) > len(symbol):
		r, _ := utf8.DecodeRuneInString(input[len(symbol):])
		if unicode.IsSpace(r) {
			return input[len(symbol):], true
		}
	case first == len(input)-len(symbol) && first > 0:
		r, _ := utf8.DecodeLastRuneInString(input[:first])
		if unicode.IsSpace(r) {
			return input[:first], true
		}
	}
	return input, false
}

func fieldValue(t *scrobble.Track, field string) *string {
	switch field {
	case "track":
		return &t.Title
	case "artist":
		return &t.Artist
	case "album":
		return &t.Album
	case "albumArtist":
		return &t.AlbumArtist
	}
	panic("clean: unknown field " + field)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
