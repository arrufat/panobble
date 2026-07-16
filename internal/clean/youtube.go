package clean

import "strings"

// ParseYoutubeTitle extracts (artist, track) from a YouTube video title.
// Port of pano's MetadataUtils.parseYoutubeTitle (from web-scrobbler).
// Empty strings mean "not found".
func ParseYoutubeTitle(videoTitle string) (artist, track string) {
	if videoTitle == "" {
		return "", ""
	}

	title := videoTitle
	for _, r := range precleaners {
		title = r.apply(title)
	}

	for _, e := range extractorsPre {
		if m := e.re.FindStringSubmatch(title); m != nil {
			artist = m[e.artistGroup]
			track = m[e.trackGroup]
			break
		}
	}

	if artist == "" || track == "" {
		artist, track = splitString(title)
	}

	if artist == "" || track == "" {
		for _, e := range extractorsPost {
			if m := e.re.FindStringSubmatch(title); m != nil {
				artist = m[e.artistGroup]
				track = m[e.trackGroup]
				break
			}
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
// separator (port of pano's splitString/findSeparator).
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

// SanitizeAlbum ports MetadataUtils.sanitizeAlbum: "unknown"-style
// placeholders become empty.
func SanitizeAlbum(album string) string {
	if metaUnknown[strings.ToLower(album)] {
		return ""
	}
	return album
}

// SanitizeArtist ports MetadataUtils.sanitizeArtist.
func SanitizeArtist(artist string) string {
	if artistSpam[strings.ToLower(artist)] {
		return ""
	}
	return artist
}

// SanitizeAlbumArtist ports MetadataUtils.sanitizeAlbumArtist.
func SanitizeAlbumArtist(albumArtist string) string {
	a := SanitizeAlbum(albumArtist)
	if strings.EqualFold(a, "VA") {
		return "Various Artists"
	}
	return a
}
