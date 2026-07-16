// Package mpris watches MPRIS players on the session bus and normalizes
// their metadata and playback state into events.
//
// Ported from pano-scrobbler: MetadataTransforms.jvm.kt (host normalization,
// wildcard domains, Spotify ad quirk), PlaybackInfoTransforms.jvm.kt and
// DesktopMediaListener.kt (playback quirks), and DesktopStuff.normalizeAppId.
package mpris

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/arrufat/panobble/rules"
	"github.com/godbus/dbus/v5"
)

const busPrefix = "org.mpris.MediaPlayer2."
const kdeconnectBus = "org.mpris.MediaPlayer2.kdeconnect"

var wildcardDomains []string

func init() {
	if err := json.Unmarshal(rules.WildcardDomains, &wildcardDomains); err != nil {
		panic("mpris: bad wildcard_domains.json: " + err.Error())
	}
}

// Metadata is the normalized per-track metadata.
type Metadata struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	TrackNumber int
	Duration    time.Duration // 0 = unknown
	URLHost     string        // normalized; "" if no xesam:url
}

type PlaybackStatus int

const (
	StatusUnknown PlaybackStatus = iota
	StatusPlaying
	StatusPaused
	StatusStopped
)

func ParsePlaybackStatus(s string) PlaybackStatus {
	switch s {
	case "Playing":
		return StatusPlaying
	case "Paused":
		return StatusPaused
	case "Stopped":
		return StatusStopped
	}
	return StatusUnknown
}

// NormalizeAppID collapses kdeconnect bus names to one id and strips
// chromium-style trailing ".instanceNNN" segments.
func NormalizeAppID(busName string) string {
	if strings.HasPrefix(busName, kdeconnectBus) {
		return kdeconnectBus
	}
	if i := strings.LastIndex(busName, "."); i >= 0 &&
		strings.HasPrefix(busName[i+1:], "instance") {
		return busName[:i]
	}
	return busName
}

// MatchesApp reports whether the normalized app id matches a config entry.
// Entries may omit the org.mpris.MediaPlayer2. prefix.
func MatchesApp(normalizedID string, entries []string) bool {
	short := strings.TrimPrefix(normalizedID, busPrefix)
	for _, e := range entries {
		if e == normalizedID || e == short {
			return true
		}
	}
	return false
}

// NormalizeURLHost extracts and normalizes the hostname from xesam:url:
// strip www., then collapse against the wildcard-domains list.
func NormalizeURLHost(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	for _, wd := range wildcardDomains {
		if strings.HasSuffix(host, wd) {
			return wd
		}
	}
	return host
}

// metadataFromMap converts an MPRIS Metadata dict into Metadata.
func metadataFromMap(m map[string]dbus.Variant) Metadata {
	md := Metadata{
		Title:       str(m, "xesam:title"),
		Album:       str(m, "xesam:album"),
		Artist:      firstOrJoined(m, "xesam:artist"),
		AlbumArtist: firstOrJoined(m, "xesam:albumArtist"),
		URLHost:     NormalizeURLHost(str(m, "xesam:url")),
	}
	if v, ok := m["xesam:trackNumber"]; ok {
		if n, ok := v.Value().(int32); ok {
			md.TrackNumber = int(n)
		}
	}
	if v, ok := m["mpris:length"]; ok {
		if us := asInt64(v.Value()); us > 0 {
			md.Duration = time.Duration(us) * time.Microsecond
		}
	}
	md.Title = strings.TrimSpace(md.Title)
	md.Artist = strings.TrimSpace(md.Artist)
	md.Album = strings.TrimSpace(md.Album)
	md.AlbumArtist = strings.TrimSpace(md.AlbumArtist)
	return md
}

// IsSpotifyAd detects Spotify ads by their metadata shape.
func (md Metadata) IsSpotifyAd() bool {
	return md.Title == "Advertisement" &&
		md.Artist != "" &&
		md.Artist == md.AlbumArtist &&
		md.Album == ""
}

func str(m map[string]dbus.Variant, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.Value().(string); ok {
			return s
		}
	}
	return ""
}

// xesam:artist is a list of strings in the spec, but some players send a
// plain string.
func firstOrJoined(m map[string]dbus.Variant, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch val := v.Value().(type) {
	case string:
		return val
	case []string:
		return strings.Join(val, ", ")
	case []dbus.Variant:
		parts := make([]string, 0, len(val))
		for _, p := range val {
			if s, ok := p.Value().(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int32:
		return int64(n)
	case uint32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
