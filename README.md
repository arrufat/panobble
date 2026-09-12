# panobble

A minimal MPRIS scrobbler daemon for Linux, submitting to Last.fm and/or
ListenBrainz. It ports the scrobbling core of
[pano-scrobbler](https://github.com/kawaiiDango/pano-scrobbler):
its metadata cleanup (YouTube title parsing, remaster/explicit stripping,
user regex rules), scrobble timing rules, and offline queue — without the GUI.

## Install

```sh
go build -o ~/.local/bin/panobble ./cmd/panobble
```

## Setup

1. Copy `contrib/config.example.toml` to `~/.config/panobble/config.toml`
   and configure at least one scrobble target (both work simultaneously):
   - **Last.fm**: create an API account at
     <https://www.last.fm/api/account/create>, fill in
     `api_key` / `api_secret`, then log in (opens a browser): `panobble auth`
   - **ListenBrainz**: paste your user token from
     <https://listenbrainz.org/settings/> into `[listenbrainz].token`
     (`api_url` supports self-hosted instances)
2. Discover your players and add them to `[players].allowed`:
   `panobble players`
3. Run it: `panobble daemon` — or install the systemd user unit:

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
| `panobble version` | print the version |

Set `PANOBBLE_DEBUG=1` for debug logs.

## Desktop notification

With `[notifications] enabled = true` the daemon shows a resident desktop
notification (freedesktop `org.freedesktop.Notifications`, tested on GNOME
Shell) for the track being scrobbled: artist, title and album, a `✓` once
the play has qualified, and — when Last.fm is configured — your scrobble
count for the track, whether you love it, a Love/Unlove button and a
button that opens the track page in your browser. The
notification is updated in place on track change and removed when playback
stops, the player exits, or the daemon shuts down. Pauses leave it as is.
Set `silent = true` to skip the banner: the notification is only added to
the notification list (GNOME shows low-urgency notifications that way).

## How it decides what to scrobble

- Only players in `[players].allowed` (ids normalized: chromium's
  `.instanceNNN` suffix is stripped). Hostnames in
  `[players].blocked_hostnames` never scrobble.
- A track *qualifies* after `min(50% of its duration, 4 min)`, minimum 30s
  (also the delay when the duration is unknown); paused time doesn't count.
  The scrobble is committed when the play actually
  ends — track change, stop, player exit, replay, or daemon shutdown — so
  last.fm never shows a track as both scrobbled and now-playing. Qualified
  tracks are journaled on disk, so a crash before the commit loses nothing.
- Tracks are cleaned first: placeholder metadata dropped, `(2004 Remaster)`
  and `(Explicit)` suffixes stripped, YouTube video titles parsed into
  artist/track, then your `[[rule]]` regex edits —
  `contrib/config.example.toml` has examples, including block rules.
- By default a track only scrobbles when artist, title **and album** are all
  present (`require_album`, on by default) — the heuristic that skips
  YouTube *videos* while scrobbling YT Music. Turn it off if your players
  report untagged files or radio streams.
- Failed submissions queue in `$XDG_DATA_HOME/panobble/pending.jsonl` and
  retry on start, hourly, and after the next successful scrobble. Queue
  entries are per service, so an outage at one target never blocks or
  duplicates scrobbles at the other.

## Rule data

The regex rule set lives in `rules/*.json` as plain, RE2-compatible JSON —
deliberately language-neutral so it can be reused by ports (each file notes
the pano-scrobbler source it was extracted from).

## License

GPL-3.0, as it derives logic from pano-scrobbler. Test fixtures in
`testdata/` originate from web-scrobbler via pano-scrobbler. The Last.fm
icon in `cmd/panobble/icons/` is from
[Font Awesome Free](https://fontawesome.com) (CC BY 4.0); Last.fm is a
trademark of Last.fm Ltd, used only to indicate the integration.
