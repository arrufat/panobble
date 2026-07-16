# panobble

A minimal MPRIS → Last.fm scrobbler daemon for Linux, porting the scrobbling
core of [pano-scrobbler](https://github.com/kawaiiDango/pano-scrobbler):
its metadata cleanup (YouTube title parsing, remaster/explicit stripping,
user regex rules), scrobble timing rules, and offline queue — without the GUI.

## Install

```sh
go build -o ~/.local/bin/panobble ./cmd/panobble
```

## Setup

1. Create a Last.fm API account: <https://www.last.fm/api/account/create>
2. Copy `contrib/config.example.toml` to `~/.config/panobble/config.toml`
   and fill in `api_key` / `api_secret`.
3. Log in (opens a browser): `panobble auth`
4. Discover your players and add them to `[players].allowed`:
   `panobble players`
5. Run it: `panobble daemon` — or install the systemd user unit:

```sh
cp contrib/panobble.service ~/.config/systemd/user/
systemctl --user enable --now panobble
```

## Commands

| command | |
|---|---|
| `panobble daemon` | run the scrobbler in the foreground |
| `panobble auth` | log in to Last.fm |
| `panobble players` | list current MPRIS players and their config ids |
| `panobble parse [--host H] ARTIST TITLE [ALBUM]` | dry-run the cleanup pipeline |
| `panobble pending [--flush]` | list / submit queued offline scrobbles |

Set `PANOBBLE_DEBUG=1` for debug logs.

## How it decides what to scrobble

- Only players in `[players].allowed` (ids normalized: chromium's
  `.instanceNNN` suffix is stripped).
- A track scrobbles after `min(50% of its duration, 4 min)`, minimum 30s,
  with pause time accounted — the same rules as pano-scrobbler and the
  Last.fm guidelines.
- Tracks are cleaned first: placeholder metadata dropped, `(2004 Remaster)`
  and `(Explicit)` suffixes stripped, YouTube video titles parsed into
  artist/track, then your `[[rule]]` regex edits.
- By default a track only scrobbles when artist, title **and album** are all
  present (`require_album`, on by default) — the heuristic that skips
  YouTube *videos* while scrobbling YT Music. Turn it off if your players
  report untagged files or radio streams.
- Failed submissions queue in `$XDG_DATA_HOME/panobble/pending.jsonl` and
  retry on start, hourly, and after the next successful scrobble.

## Rule data

The regex rule set lives in `rules/*.json` as plain, RE2-compatible JSON —
deliberately language-neutral so it can be reused by ports (each file notes
the pano-scrobbler source it was extracted from).

## License

GPL-3.0, as it derives logic from pano-scrobbler. Test fixtures in
`testdata/` originate from web-scrobbler via pano-scrobbler.
