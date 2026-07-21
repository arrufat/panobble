package clean

import "strings"

// ParseYoutubeTitle extracts (artist, track) from a YouTube video title.
// Empty strings mean "not found".
func ParseYoutubeTitle(videoTitle string) (artist, track string) {
	if videoTitle == "" {
		return "", ""
	}

	title := videoTitle
	for _, r := range precleaners {
		title = r.apply(title)
	}

	artist, track, _ = runExtractors(extractorsPre, title)

	if artist == "" || track == "" {
		artist, track = splitString(title)
	}

	if artist == "" || track == "" {
		if a, t, ok := runExtractors(extractorsPost, title); ok {
			artist, track = a, t
		}
	}

	if artist == "" || track == "" {
		track = title
	}

	track = CleanYoutubeTrack(track)
	if artist != "" {
		artist = cleanYoutubeArtist(artist)
	}
	return artist, track
}

// runExtractors returns the artist/track groups of the first matching
// extractor; ok reports whether any matched.
func runExtractors(es []compiledExtractor, title string) (artist, track string, ok bool) {
	for _, e := range es {
		if m := e.re.FindStringSubmatch(title); m != nil {
			return m[e.artistGroup], m[e.trackGroup], true
		}
	}
	return "", "", false
}

// CleanYoutubeTrack strips YouTube suffixes/prefixes from a track title.
func CleanYoutubeTrack(track string) string {
	for _, r := range trackFilters {
		track = r.apply(track)
	}
	return track
}

func cleanYoutubeArtist(artist string) string {
	for _, r := range artistFilters {
		artist = r.apply(artist)
	}
	return artist
}

// splitString splits at the first occurrence of the highest-priority
// separator.
func splitString(s string) (first, second string) {
	if s == "" {
		return "", ""
	}
	for _, sep := range separators {
		if i := strings.Index(s, sep); i > -1 {
			return s[:i], s[i+len(sep):]
		}
	}
	return "", ""
}

// SanitizeAlbum turns "unknown"-style placeholders into empty.
func SanitizeAlbum(album string) string {
	if metaUnknown[strings.ToLower(album)] {
		return ""
	}
	return album
}

// SanitizeArtist drops known spam artist values.
func SanitizeArtist(artist string) string {
	if artistSpam[strings.ToLower(artist)] {
		return ""
	}
	return artist
}

// SanitizeAlbumArtist sanitizes placeholders and canonicalizes "VA" to
// "Various Artists".
func SanitizeAlbumArtist(albumArtist string) string {
	a := SanitizeAlbum(albumArtist)
	if strings.EqualFold(a, "VA") {
		return "Various Artists"
	}
	return a
}
