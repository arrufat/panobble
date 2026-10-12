package clean

import (
	"testing"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/scrobble"
)

func defaultPipeline(t *testing.T, rules ...config.Rule) *Pipeline {
	t.Helper()
	p, err := NewPipeline(config.Default().Cleanup, rules)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPresetRemastered(t *testing.T) {
	p := defaultPipeline(t)
	cases := []struct{ in, want string }{
		{"Song (2004 Remaster)", "Song"},
		{"Song (Remastered 2011)", "Song"},
		{"Song [2004 Remastered Version]", "Song"},
		{"Song - 2004 remaster", "Song"},
		{"Song / Remastered", "Song"},
		{"Remaster Class", "Remaster Class"}, // no false positive
	}
	for _, c := range cases {
		got, res := p.Clean(scrobble.Track{Artist: "A", Title: c.in, Album: "Album"}, "")
		if res.Blocked || res.ParseFailed {
			t.Fatalf("unexpected result for %q: %+v", c.in, res)
		}
		if got.Title != c.want {
			t.Errorf("remastered(%q) = %q, want %q", c.in, got.Title, c.want)
		}
	}
}

func TestPresetRemasteredAlbumField(t *testing.T) {
	p := defaultPipeline(t)
	got, _ := p.Clean(scrobble.Track{Artist: "A", Title: "T", Album: "Album (2004 Remaster)"}, "")
	if got.Album != "Album" {
		t.Errorf("album = %q, want %q", got.Album, "Album")
	}
}

func TestPresetExplicit(t *testing.T) {
	p := defaultPipeline(t)
	cases := []struct{ in, want string }{
		{"Song (Explicit)", "Song"},
		{"Song - Explicit", "Song"},
		{"Song [Clean]", "Song"},
		{"Song 🅴", "Song"}, // adjacent space removed by the pipeline's final trim
		{"🅴 Song", "Song"},
		{"Song🅴", "Song🅴"}, // no whitespace boundary: untouched
	}
	for _, c := range cases {
		got, _ := p.Clean(scrobble.Track{Artist: "A", Title: c.in}, "")
		if got.Title != c.want {
			t.Errorf("explicit(%q) = %q, want %q", c.in, got.Title, c.want)
		}
	}
}

func TestPresetSingleEPGatedToAppleMusic(t *testing.T) {
	p := defaultPipeline(t)
	in := scrobble.Track{Artist: "A", Title: "T", Album: "Great Album - Single"}

	got, _ := p.Clean(in, "music.apple.com")
	if got.Album != "Great Album" {
		t.Errorf("apple host: album = %q, want %q", got.Album, "Great Album")
	}
	got, _ = p.Clean(in, "music.youtube.com")
	if got.Album != "Great Album - Single" {
		t.Errorf("non-apple host: album should be untouched, got %q", got.Album)
	}
}

func TestParseTitleOnYoutube(t *testing.T) {
	p := defaultPipeline(t)

	// No album, plain youtube: parse the title.
	got, res := p.Clean(scrobble.Track{Artist: "SomeChannel", Title: "Artist - Track (Official Video)"}, "youtube.com")
	if res.ParseFailed {
		t.Fatal("unexpected parse failure")
	}
	if got.Artist != "Artist" || got.Title != "Track" {
		t.Errorf("got artist=%q title=%q", got.Artist, got.Title)
	}

	// Unparseable title on strict youtube: skip the scrobble.
	_, res = p.Clean(scrobble.Track{Artist: "SomeChannel", Title: "JustOneWord"}, "youtube.com")
	if !res.ParseFailed {
		t.Error("expected ParseFailed on strict youtube host")
	}

	// Same on music.youtube.com (fallback variant): no failure, keep as-is.
	got, res = p.Clean(scrobble.Track{Artist: "SomeChannel", Title: "JustOneWord"}, "music.youtube.com")
	if res.ParseFailed || got.Title != "JustOneWord" {
		t.Errorf("fallback should keep track: %+v %+v", got, res)
	}

	// With album present: no parsing.
	got, _ = p.Clean(scrobble.Track{Artist: "Artist", Title: "A - B", Album: "Album"}, "youtube.com")
	if got.Title != "A - B" {
		t.Errorf("album present: title should be untouched, got %q", got.Title)
	}
}

func TestParseTitleTopicSuffix(t *testing.T) {
	p := defaultPipeline(t)
	got, _ := p.Clean(scrobble.Track{Artist: "Cool Artist - Topic", Title: "Track"}, "music.youtube.com")
	if got.Artist != "Cool Artist" {
		t.Errorf("artist = %q, want %q", got.Artist, "Cool Artist")
	}
}

func TestSanitize(t *testing.T) {
	p := defaultPipeline(t)
	got, _ := p.Clean(scrobble.Track{
		Artist: "A", Title: "T",
		Album: "[unknown album]", AlbumArtist: "va",
	}, "")
	if got.Album != "" {
		t.Errorf("album = %q, want empty", got.Album)
	}
	if got.AlbumArtist != "Various Artists" {
		t.Errorf("albumArtist = %q, want Various Artists", got.AlbumArtist)
	}
}

func TestUserRuleReplaceAndBlock(t *testing.T) {
	p := defaultPipeline(t,
		config.Rule{
			Name:             "strip feat",
			Search:           map[string]string{"track": `\s*\(feat\. [^)]*\)$`},
			Replacement:      map[string]string{"track": ""},
			ContinueMatching: true,
		},
		config.Rule{
			Name:   "block asmr",
			Search: map[string]string{"track": `(?:^|\s)asmr(?:\s|$)`},
			Block:  true,
		},
	)

	got, res := p.Clean(scrobble.Track{Artist: "A", Title: "Song (feat. B)"}, "")
	if res.Blocked || got.Title != "Song" {
		t.Errorf("got %+v %+v", got, res)
	}

	_, res = p.Clean(scrobble.Track{Artist: "A", Title: "rainy ASMR sounds"}, "")
	if !res.Blocked || res.BlockReason != "block asmr" {
		t.Errorf("expected block, got %+v", res)
	}
}

func TestUserRulePerAppGating(t *testing.T) {
	p := defaultPipeline(t, config.Rule{
		Name:        "only for mpv",
		Search:      map[string]string{"track": `^x`},
		Replacement: map[string]string{"track": "y"},
		Apps:        []string{"org.mpris.MediaPlayer2.mpv"},
	})
	got, _ := p.Clean(scrobble.Track{Artist: "A", Title: "xyz", AppID: "org.mpris.MediaPlayer2.spotify"}, "")
	if got.Title != "xyz" {
		t.Errorf("rule should not apply to spotify, got %q", got.Title)
	}
	got, _ = p.Clean(scrobble.Track{Artist: "A", Title: "xyz", AppID: "org.mpris.MediaPlayer2.mpv"}, "")
	if got.Title != "yyz" {
		t.Errorf("rule should apply to mpv, got %q", got.Title)
	}
}

func TestSecondPassAfterPresets(t *testing.T) {
	// A user rule that cleans up the parse output: only matches after
	// parse_title has split artist/track.
	p := defaultPipeline(t, config.Rule{
		Name:        "strip star",
		Search:      map[string]string{"artist": `\s*★$`},
		Replacement: map[string]string{"artist": ""},
	})
	got, _ := p.Clean(scrobble.Track{Artist: "Channel", Title: "Cool Artist ★ - Track"}, "youtube.com")
	if got.Artist != "Cool Artist" || got.Title != "Track" {
		t.Errorf("got artist=%q title=%q", got.Artist, got.Title)
	}
}
