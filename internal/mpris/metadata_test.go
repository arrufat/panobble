package mpris

import "testing"

func TestNormalizeAppID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"org.mpris.MediaPlayer2.spotify", "org.mpris.MediaPlayer2.spotify"},
		{"org.mpris.MediaPlayer2.chromium.instance5160", "org.mpris.MediaPlayer2.chromium"},
		{"org.mpris.MediaPlayer2.firefox.instance_1_23", "org.mpris.MediaPlayer2.firefox"},
		{"org.mpris.MediaPlayer2.kdeconnect.mpris_000001", "org.mpris.MediaPlayer2.kdeconnect"},
		{"org.mpris.MediaPlayer2.mpv", "org.mpris.MediaPlayer2.mpv"},
	}
	for _, c := range cases {
		if got := NormalizeAppID(c.in); got != c.want {
			t.Errorf("NormalizeAppID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchesApp(t *testing.T) {
	entries := []string{"spotify", "org.mpris.MediaPlayer2.mpv"}
	if !MatchesApp("org.mpris.MediaPlayer2.spotify", entries) {
		t.Error("short entry should match full id")
	}
	if !MatchesApp("org.mpris.MediaPlayer2.mpv", entries) {
		t.Error("full entry should match full id")
	}
	if MatchesApp("org.mpris.MediaPlayer2.vlc", entries) {
		t.Error("vlc should not match")
	}
}

func TestNormalizeURLHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://www.youtube.com/watch?v=x", "youtube.com"},
		{"https://music.youtube.com/watch?v=x", "music.youtube.com"},
		{"https://daftpunk.bandcamp.com/track/x", "bandcamp.com"},
		{"https://www.bbc.co.uk/sounds", "bbc.co.uk"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeURLHost(c.in); got != c.want {
			t.Errorf("NormalizeURLHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
