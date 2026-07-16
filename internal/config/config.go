// Package config loads panobble's TOML configuration and resolves XDG paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Lastfm struct {
	APIKey    string `toml:"api_key"`
	APISecret string `toml:"api_secret"`
}

type Scrobble struct {
	DelayPercent    int  `toml:"delay_percent"`
	DelayMaxSecs    int  `toml:"delay_max_secs"`
	MinDurationSecs int  `toml:"min_duration_secs"`
	NowPlaying      bool `toml:"now_playing"`
}

type Players struct {
	Allowed []string `toml:"allowed"`
	// RequireAlbum drops tracks whose album metadata is missing. On by
	// default: real music players (and YT Music in a browser) report an
	// album; YouTube videos and other junk media do not.
	RequireAlbum     bool     `toml:"require_album"`
	BlockedHostnames []string `toml:"blocked_hostnames"`
}

type Cleanup struct {
	Presets        []string `toml:"presets"`
	ParseTitleApps []string `toml:"parse_title_apps"`
}

// Rule is a user-defined regex edit, a port of pano-scrobbler's RegexEdit.
type Rule struct {
	Name             string            `toml:"name"`
	Search           map[string]string `toml:"search"`      // field -> pattern; fields: track, artist, album, albumArtist
	Replacement      map[string]string `toml:"replacement"` // field -> replacement
	Apps             []string          `toml:"apps"`
	Hosts            []string          `toml:"hosts"`
	CaseSensitive    bool              `toml:"case_sensitive"`
	ReplaceAll       bool              `toml:"replace_all"`
	Block            bool              `toml:"block"`
	ContinueMatching bool              `toml:"continue_matching"`
}

type Config struct {
	Lastfm   Lastfm   `toml:"lastfm"`
	Scrobble Scrobble `toml:"scrobble"`
	Players  Players  `toml:"players"`
	Cleanup  Cleanup  `toml:"cleanup"`
	Rules    []Rule   `toml:"rule"`
}

// Defaults mirror pano-scrobbler's MainPrefs defaults.
func Default() Config {
	return Config{
		Scrobble: Scrobble{
			DelayPercent:    50,
			DelayMaxSecs:    240,
			MinDurationSecs: 30,
			NowPlaying:      true,
		},
		Players: Players{
			RequireAlbum: true,
		},
		Cleanup: Cleanup{
			Presets: []string{
				"parse_title", "parse_title_with_fallback",
				"remastered", "explicit", "single_ep",
			},
		},
	}
}

// Dir returns the config directory (~/.config/panobble).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "panobble"), nil
}

// DataDir returns the state directory ($XDG_DATA_HOME/panobble).
func DataDir() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "panobble"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "panobble"), nil
}

// Path returns the config file path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// Load reads the config file, applying defaults for missing values.
// A missing file is not an error; defaults are returned.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	return LoadFrom(path)
}

func LoadFrom(path string) (Config, error) {
	cfg := Default()
	meta, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("unknown config keys in %s: %v", path, undecoded)
	}
	cfg.clamp()
	return cfg, nil
}

// clamp applies pano's pref bounds.
func (c *Config) clamp() {
	c.Scrobble.DelayPercent = clamp(c.Scrobble.DelayPercent, 30, 95)
	c.Scrobble.DelayMaxSecs = clamp(c.Scrobble.DelayMaxSecs, 30, 360)
	c.Scrobble.MinDurationSecs = clamp(c.Scrobble.MinDurationSecs, 10, 60)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
