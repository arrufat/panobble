package clean

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Fixtures copied verbatim from pano-scrobbler (originally web-scrobbler).

// parseOverrides pins cases where pano-scrobbler's port deliberately diverges
// from the original web-scrobbler fixtures (pano's own MaiTest fails on
// these). panobble follows pano's code, not the fixtures:
//   - pano's cleanYoutubeTrack trims leading/trailing whitespace globally,
//     so "Track " becomes "Track";
//   - pano disabled the bare "-" separator, so "Artist -Track-【...】" is
//     resolved by the 【】 fallback extractor instead of a dash split.
var parseOverrides = map[string]struct{ artist, track string }{
	`should remove "(PV)" string`:          {"Artist", "Track"},
	`should remove "(MV Whatever)" string`: {"Artist", "Track"},
	`should prioritize dashes over 【】`:     {"Artist -Track", "Official Video"},
}

func TestParseYoutubeTitleFixtures(t *testing.T) {
	var cases []struct {
		Description string    `json:"description"`
		Args        []*string `json:"args"`
		Expected    struct {
			Artist *string `json:"artist"`
			Track  *string `json:"track"`
		} `json:"expected"`
	}
	loadFixture(t, "youtubeArtistTracks.json", &cases)
	if len(cases) == 0 {
		t.Fatal("no cases loaded")
	}

	for _, c := range cases {
		input := ""
		if len(c.Args) > 0 && c.Args[0] != nil {
			input = *c.Args[0]
		}
		wantArtist, wantTrack := deref(c.Expected.Artist), deref(c.Expected.Track)
		if o, ok := parseOverrides[c.Description]; ok {
			wantArtist, wantTrack = o.artist, o.track
		}
		artist, track := ParseYoutubeTitle(input)
		if artist != wantArtist || track != wantTrack {
			t.Errorf("%s:\n  input:  %q\n  got:    artist=%q track=%q\n  want:   artist=%q track=%q",
				c.Description, input, artist, track, wantArtist, wantTrack)
		}
	}
	t.Logf("%d cases", len(cases))
}

func TestCleanYoutubeTrackFixtures(t *testing.T) {
	var cases []struct {
		Description   string `json:"description"`
		FuncParameter string `json:"funcParameter"`
		ExpectedValue string `json:"expectedValue"`
	}
	loadFixture(t, "youtubeTracks.json", &cases)
	if len(cases) == 0 {
		t.Fatal("no cases loaded")
	}

	for _, c := range cases {
		if got := CleanYoutubeTrack(c.FuncParameter); got != c.ExpectedValue {
			t.Errorf("%s:\n  input: %q\n  got:   %q\n  want:  %q",
				c.Description, c.FuncParameter, got, c.ExpectedValue)
		}
	}
	t.Logf("%d cases", len(cases))
}

func loadFixture(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
