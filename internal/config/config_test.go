package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scrobble.DelayPercent != 50 || cfg.Scrobble.DelayMaxSecs != 240 ||
		cfg.Scrobble.MinDurationSecs != 30 || !cfg.Scrobble.NowPlaying {
		t.Errorf("unexpected defaults: %+v", cfg.Scrobble)
	}
	if len(cfg.Cleanup.Presets) != 5 {
		t.Errorf("expected 5 default presets, got %v", cfg.Cleanup.Presets)
	}
}

func TestLoadClampsAndParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[lastfm]
api_key = "k"
api_secret = "s"

[scrobble]
delay_percent = 99
min_duration_secs = 5

[players]
allowed = ["spotify"]
require_album = ["org.mpris.MediaPlayer2.chromium"]

[[rule]]
name = "strip feat"
search = { track = '\s*\(feat\. [^)]*\)$' }
replacement = { track = "" }
continue_matching = true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scrobble.DelayPercent != 95 {
		t.Errorf("delay_percent should clamp to 95, got %d", cfg.Scrobble.DelayPercent)
	}
	if cfg.Scrobble.MinDurationSecs != 10 {
		t.Errorf("min_duration_secs should clamp to 10, got %d", cfg.Scrobble.MinDurationSecs)
	}
	if cfg.Scrobble.DelayMaxSecs != 240 {
		t.Errorf("delay_max_secs should keep default 240, got %d", cfg.Scrobble.DelayMaxSecs)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Search["track"] == "" {
		t.Errorf("rule not parsed: %+v", cfg.Rules)
	}
	if len(cfg.Players.RequireAlbum) != 1 {
		t.Errorf("require_album not parsed: %+v", cfg.Players)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[scrobble]\ntypo_key = 1\n"), 0o644)
	if _, err := LoadFrom(path); err == nil {
		t.Error("expected error for unknown key")
	}
}
