package clean

import (
	"strings"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/scrobble"
)

// Pipeline is the configured cleanup pipeline.
type Pipeline struct {
	presetEnabled  map[string]bool
	parseTitleApps []string
	userRules      []userRule
}

// Result reports what the pipeline decided beyond the cleaned track.
type Result struct {
	Blocked     bool
	BlockReason string // rule name
	ParseFailed bool   // strict parse_title failed: skip this scrobble
}

func NewPipeline(cfg config.Cleanup, rules []config.Rule) (*Pipeline, error) {
	compiled, err := compileUserRules(rules)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		presetEnabled:  toSet(cfg.Presets),
		parseTitleApps: cfg.ParseTitleApps,
		userRules:      compiled,
	}, nil
}

// Clean runs the full pipeline, a port of pano's preprocessMetadata:
// sanitize → user rules → presets → (if only presets changed) user rules
// again over the presets' output.
func (p *Pipeline) Clean(t scrobble.Track, host string) (scrobble.Track, Result) {
	t.Artist = SanitizeArtist(t.Artist)
	t.Album = SanitizeAlbum(t.Album)
	t.AlbumArtist = SanitizeAlbumArtist(t.AlbumArtist)

	t, userEditsApplied, blockedBy, stopped := p.applyUserRules(t, host)
	if blockedBy != "" {
		return t, Result{Blocked: true, BlockReason: blockedBy}
	}

	presetsApplied := false
	if !stopped {
		var parseFailed bool
		t, presetsApplied, parseFailed = p.applyPresets(t, host, userEditsApplied)
		if parseFailed {
			return t, Result{ParseFailed: true}
		}
	}

	if !userEditsApplied && presetsApplied {
		// Second pass: let user rules clean up the presets' output.
		var blocked string
		t, _, blocked, _ = p.applyUserRules(t, host)
		if blocked != "" {
			return t, Result{Blocked: true, BlockReason: blocked}
		}
	}

	// Deviation from pano: trim the final fields. Pano can leave edge
	// whitespace behind (e.g. the 🅴 edge trim keeps the adjacent space and
	// would submit "Song ").
	t.Artist = strings.TrimSpace(t.Artist)
	t.Title = strings.TrimSpace(t.Title)
	t.Album = strings.TrimSpace(t.Album)
	t.AlbumArtist = strings.TrimSpace(t.AlbumArtist)

	return t, Result{}
}
