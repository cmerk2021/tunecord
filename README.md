# tunecord

**tunecord** is a small background service for Linux desktops that shows what you're playing as a Discord
**"Listening to …" Rich Presence**, with album art and a progress bar you can trust.

It watches [MPRIS](https://specifications.freedesktop.org/mpris-spec/latest/) media players over the D-Bus
session bus. That covers VLC, Elisa, Strawberry, Spotify, Lollypop, mpv (with mpv-mpris), Firefox/Chromium tabs,
and anything else that shows up in your desktop's media controls.

Why another one? Other MPRIS→Discord bridges have hit-or-miss progress bars: a player shows correct progress in
KDE Plasma's media controls, but the bar in Discord is wrong, frozen or missing. tunecord's main goal is a reliable
progress bar. See [How the progress bar is hardened](#how-the-progress-bar-is-hardened).

- Single static Go binary (no CGO), Linux `amd64` and `arm64`
- Runs as a systemd **user** service. Everything works from `~/.local/bin`.
- Ordered player allowlist: you decide which player wins when several are active
- Album art from the player itself, Spotify (optional), iTunes and MusicBrainz/Cover Art Archive, with caching
- Never spams Discord: coalesced, rate-limited updates
- Survives Discord restarts, player crashes and bus hiccups

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Commands](#commands)
- [Configuration reference](#configuration-reference)
- [Player names and the priority list](#player-names-and-the-priority-list)
- [How the progress bar is hardened](#how-the-progress-bar-is-hardened)
- [Diagnosing a misbehaving player with `tunecord debug`](#diagnosing-a-misbehaving-player-with-tunecord-debug)
- [Album art](#album-art)
- [Discord: Flatpak, Snap, Vesktop & co.](#discord-flatpak-snap-vesktop--co)
- [Troubleshooting](#troubleshooting)
- [Design decisions](#design-decisions)
- [Building from source](#building-from-source)

---

## Install

Download the archive for your architecture from the
[releases page](https://github.com/cmerk2021/tunecore/releases) and put the binary in `~/.local/bin`:

```sh
# pick amd64 or arm64, and the version you want
VERSION=1.0.0 ARCH=amd64
curl -LO "https://github.com/cmerk2021/tunecore/releases/download/v${VERSION}/tunecord_${VERSION}_linux_${ARCH}.tar.gz"
tar xzf "tunecord_${VERSION}_linux_${ARCH}.tar.gz"
install -Dm755 tunecord ~/.local/bin/tunecord
```

You can verify the download against `checksums.txt` from the same release (`sha256sum -c --ignore-missing checksums.txt`).

If your shell can't find `tunecord`, add `~/.local/bin` to your `PATH`:

```sh
export PATH="$HOME/.local/bin:$PATH"   # add to ~/.bashrc, ~/.zshrc or ~/.profile
fish_add_path ~/.local/bin             # fish
```

The service doesn't depend on `PATH`, because the systemd unit uses the binary's absolute path.

**Runtime requirements:** a desktop session with a D-Bus session bus (every normal desktop has one), and `systemctl`
if you want the background service or the automatic reload after `tunecord config`. Nothing else: no CGO, no
libraries, no system-wide files.

## Quick start

```sh
tunecord list              # 1. see the names of the players that are running
tunecord config            # 2. pick your players (and their priority) and options
tunecord install --user    # 3. run it in the background, now and on every login
tunecord status            # check that it's connected and what it's showing
```

`tunecord list` prints the player names exactly as they go into the allowlist:

```
NAME      INSTANCE              IDENTITY          STATUS   ALLOWLIST  ACTIVE
spotify   -                     Spotify           Playing  yes (#1)   ◀ active
vlc       vlc.instance12345     VLC media player  Paused   yes (#2)
chromium  chromium.instance987  Chromium          Playing  -
```

> tunecord does **nothing** until at least one player is in the allowlist. This is on purpose: it never
> broadcasts your browser tabs or other media without your consent.

## Commands

All commands accept `--verbose`/`-v` (debug logging) and `--config <path>` (use another config file).

| Command | What it does |
|---|---|
| `tunecord run` | Runs the service in the foreground. This is what systemd runs. It writes a pidfile to `$XDG_RUNTIME_DIR/tunecord.pid` and refuses to start if another tunecord already holds it. **SIGHUP** re-reads the config and re-initializes the D-Bus/Discord connections without exiting. **SIGINT/SIGTERM** clear the presence and exit cleanly. |
| `tunecord config` | Interactive guided setup (terminal UI). It is pre-filled with your current values and covers every option. On save it writes the config atomically (mode `0600`) and reloads the running service. |
| `tunecord list` | Lists the MPRIS players on the bus in allowlist form, marking allowlist membership/priority and the active player. `--active` prints only the name of the active player and exits `1` if there is none. `--json` gives machine-readable output (combine with `--active` for one object, or `null`). |
| `tunecord debug [player…]` | Live per-player diagnostics, refreshed every second until Ctrl+C. Shows the raw D-Bus values **and Go types** of `Position`, `mpris:length`, `Rate`, `PlaybackStatus`, `mpris:artUrl` and all metadata keys, plus tunecord's progress-bar verdict and why. `--once` observes for about 3 s (enough to detect a stuck position) and prints one report. `--interval` changes the refresh rate. |
| `tunecord install --user` | Writes `~/.config/systemd/user/tunecord.service` with the **absolute** path of the binary, then runs `daemon-reload` and `enable --now`. It's idempotent: re-run it after moving the binary. `--user` is required. |
| `tunecord uninstall --user` | Stops, disables and removes the unit, then reloads systemd. Your config and art cache are kept. |
| `tunecord status` | Shows whether the service is running (systemd or foreground), the D-Bus and Discord IPC connections, the current player and track, the progress-bar mode (`real`/`estimated`/`none`) with the reason, the album art and warnings (e.g. empty allowlist). `--json` is available. Exits `3` when the service isn't running. |
| `tunecord version` | Prints the version, commit and build date. |

### Which player does `list --active` print?

The one tunecord would show: the highest-priority allowlisted player that is playing (or paused, if
`show_when_paused` is on). If no allowlisted player qualifies, it prints the first player that is `Playing`, so the
command is also useful before you've configured anything. Exit code `1` means nothing is active.

## Configuration reference

Location: `~/.config/tunecord/config.toml` (or `$XDG_CONFIG_HOME/tunecord/config.toml`). `tunecord config` writes
it for you. If it doesn't exist, `run` uses the defaults below and logs a hint. Since the allowlist defaults to
empty, nothing is shown until you configure it.

```toml
[general]
log_level = "info"            # debug | info | warn | error  (--verbose overrides this)

[discord]
client_id = "1525387248332443781"   # Discord application ID; its name appears as "Listening to <name>"

[players]
# Ordered by priority, top = highest. Names from `tunecord list` (or a player's Identity).
allowlist = []

[polling]
idle_interval = "5s"          # how often to look when nothing is playing
playing_interval = "1s"       # how often to verify the position while playing
                              # both: min 500ms, max 60s

[presence]
status_display = "artist"     # what shows under your name: "name" | "artist" | "song"
show_album = false            # album as hover text on the art; when false the album is never sent
show_when_paused = true       # keep the card (no bar, marked paused) while paused; false = clear it
default_art_asset = "music"   # Discord asset key or https URL used when no art is found
paused_asset = ""             # optional small image (asset key/URL) shown while paused;
                              # empty = append " · Paused" to the artist line instead

[progress]
fallback = "estimate"         # when the player's position is unreliable: "estimate" | "skip"
seek_threshold = "2.5s"       # deviation from the predicted position that counts as a seek (500ms–1m)

[art]
sources = ["mpris", "spotify", "itunes", "musicbrainz"]   # lookup order; remove entries to disable them

[art.spotify]                 # optional; both or neither
client_id = ""
client_secret = ""
```

**Validation.** The config is validated on load. Unknown keys produce a warning (`config: unknown config key "…" (ignored)`). Bad
values produce a clear error that names the key and the allowed range. `tunecord run` refuses to start with an
invalid config. On SIGHUP an invalid config is rejected and the previous one stays in use.

**`status_display`** controls what appears under your name in the member list, next to the music icon:

| value | shows | example |
|---|---|---|
| `artist` (default) | the state line (artists) | *Listening to Daft Punk* |
| `song` | the details line (title) | *Listening to Get Lucky* |
| `name` | the application name | *Listening to tunecord* |

**Reloading.** The service reads the config **only at startup**. There is no file watcher. `tunecord config`
reloads it for you: it restarts `tunecord.service` if the unit is active, otherwise it sends SIGHUP to a
pidfile-tracked `tunecord run`, otherwise it tells you the service isn't running. After a manual edit, run
`systemctl --user restart tunecord` (or `kill -HUP $(cat $XDG_RUNTIME_DIR/tunecord.pid)`).

**Files**

| Path | Purpose |
|---|---|
| `$XDG_CONFIG_HOME/tunecord/config.toml` (`~/.config/…`) | config, mode `0600` (may contain Spotify secrets) |
| `$XDG_CACHE_HOME/tunecord/art.json` (`~/.cache/…`) | album art cache |
| `$XDG_RUNTIME_DIR/tunecord.pid` | pidfile of the running service |
| `$XDG_RUNTIME_DIR/tunecord.status.json` | live state published for `tunecord status` |
| `$XDG_CONFIG_HOME/systemd/user/tunecord.service` | unit written by `install --user` |

## Player names and the priority list

Each player is identified by the suffix of its MPRIS bus name:

| bus name | allowlist name |
|---|---|
| `org.mpris.MediaPlayer2.vlc` | `vlc` |
| `org.mpris.MediaPlayer2.vlc.instance12345` | `vlc` (the instance suffix is ignored) |
| `org.mpris.MediaPlayer2.chromium.instance987` | `chromium` |
| `org.mpris.MediaPlayer2.spotify` | `spotify` |

Matching is case-insensitive. An entry also matches the full suffix (`vlc.instance12345`) or the player's
`Identity` property (`VLC media player`, `Elisa`).

**The allowlist is an ordered priority list, top = highest.**

1. The highest-listed player that is **Playing** drives the presence.
2. If none is playing, the highest-listed **Paused** player is shown, if `show_when_paused = true`.
3. Players not on the allowlist are ignored completely.
4. If the allowlist is empty, tunecord shows nothing and warns about it in the log and in `tunecord status`. It
   never falls back to "everything".

Example: with `allowlist = ["spotify", "vlc"]`, if Spotify is paused and VLC is playing, VLC is shown. When you resume
Spotify, it takes over. If both are paused, Spotify is shown (paused).

Players appearing and disappearing are tracked through D-Bus `NameOwnerChanged` signals, so there is no bus polling.
`PropertiesChanged`, `Seeked` and `NameOwnerChanged` trigger an immediate re-evaluation regardless of the poll
interval. Polling (`idle_interval`/`playing_interval`) only exists to re-read `Position`, which MPRIS never
announces.

## How the progress bar is hardened

Discord draws the bar from two absolute timestamps, `start` and `end`. Getting them right is harder than it looks.
These are the failure modes tunecord handles (each one is covered by unit tests in `internal/progress`):

1. **`Position` never emits `PropertiesChanged`** (that's the MPRIS spec). Libraries that read it from a cached
   property map get a stale value. tunecord reads it with a direct `org.freedesktop.DBus.Properties.Get` call
   every time. It also subscribes to the `Seeked` signal and re-syncs immediately when a player announces a seek.
2. **Inconsistent D-Bus types.** `Position`, `mpris:length` and `Seeked` should be `int64` microseconds. In practice
   they arrive as `uint64`, `int32`, `uint32`, `double`, nested variants, or even strings. A single table-tested
   normalizer (`mpris.ToMicroseconds`) accepts all of them.
3. **Jitter and Discord's rate limit** (about 5 updates per 20 s). tunecord does not recompute `start = now - position`
   on every poll. It keeps an **anchor** (`anchorWallTime`, `anchorPosition`, `rate`) and predicts
   `position = anchorPosition + (now - anchorWallTime) × rate`. A new activity is pushed only when:
   - the track changes (by `mpris:trackid`, or by title + artists),
   - the playback status changes,
   - a seek is detected (a `Seeked` signal, or the reported position deviates from the prediction by more than
     `seek_threshold`),
   - the rate changes,
   - or displayed metadata changes (e.g. album art resolved).

   On top of that, pushes are spaced at least **4 s** apart (and at most 5 per 20 s). Rapid changes are coalesced and
   only the latest state is sent. Identical activities (timestamps within 1 s) are never re-sent.
4. **Playback rate ≠ 1.0.** `Rate` is read and the bar is scaled: `start = now − position / rate`,
   `end = now + (length − position) / rate`. A rate change re-anchors.
5. **Bad data.**
   - A length that is ≤ 0, missing, or longer than 24 h (streams) gives **no bar**. The presence is still shown.
   - A position < 0 or > length is clamped. If it overshoots by more than 2 s, the position is considered unreliable.
   - A position stuck at 0 (or not advancing) while Playing is marked unreliable after a **3 s grace period and at
     least 3 samples**.
6. **Stale positions across transitions.** Every transition re-reads the position fresh. On a track change tunecord
   waits a **400 ms settle delay** and re-reads before the first push, because some players briefly report the
   previous track's position. A position past the new length, or one that is just the old track's position, is
   ignored and the new track starts at 0.
7. **Paused.** While paused, the activity is sent **without timestamps** (the bar disappears), with " · Paused"
   appended to the state line (or the `paused_asset` small image). On resume the bar is re-anchored from a fresh read.

### Unreliable-position fallback

When a player's position can't be trusted, `progress.fallback` decides what happens:

- **`estimate`** (default): tunecord runs its own timer. It is anchored when the track started (position 0) or,
  after a resume, at the last known good position. `status` and `debug` report the mode as `estimated`.
- **`skip`**: no progress bar for that track.

If the player starts reporting a sane, advancing position again mid-track (2 consecutive consistent samples),
tunecord switches back to real data and re-anchors.

## Diagnosing a misbehaving player with `tunecord debug`

Start playback, then run:

```sh
tunecord debug            # all players, live
tunecord debug vlc        # only VLC
tunecord debug --once     # one report after ~3 s of observation (good for bug reports)
```

Example output:

```
━━ smokeplayer.instance9  [allowlisted, priority #1]
  allowlist name : smokeplayer
  PlaybackStatus : Playing  (string)
  Position       : 34036887  (string) = 0:34
  mpris:length   : 200000000  (int32) = 3:20
  Rate           : 1  (float64)
  mpris:artUrl   : file:///tmp/cover.png  (string)
                   → local file (Discord cannot load it); tunecord will look art up online
  ...
  ── progress bar verdict (4 samples over 3s)
     RELIABLE: real progress bar from the player's position
     why: player reports a sane, advancing position
     note: Position is string instead of int64 (non-standard; tunecord normalizes it)
```

How to read the verdict:

| Verdict | Meaning |
|---|---|
| `RELIABLE` | the player's position is sane and advancing, so the bar uses it |
| `ESTIMATED` | the position is unreliable (stuck at 0, overshooting, unreadable…); the reason is shown and tunecord's own timer is used |
| `NO BAR` | paused/stopped, no valid length (stream), or unreliable with `fallback = "skip"` |

If the bar in Discord is wrong but `debug` says `RELIABLE`, compare the predicted `start`/`end` with Discord and
[open an issue](https://github.com/cmerk2021/tunecore/issues) with the `debug --once` output.

## Album art

For each track, the first source that succeeds wins (order configurable through `art.sources`):

1. **`mpris`**: the player's `mpris:artUrl`, but only if it is a **public** `http(s)` URL. `file://`, `data:`,
   localhost and private-network URLs are skipped, because Discord can't fetch them. Spotify's `i.scdn.co` URLs are
   used directly.
2. **`spotify`** (optional): Spotify Web API search with the Client Credentials flow. The token is cached until it
   expires. It searches `track:"…" artist:"…"` and takes the 640 px (or 300 px) album image.
3. **`itunes`**: iTunes Search API, no key needed. Artwork is upscaled from `100x100bb` to `600x600bb`.
4. **`musicbrainz`**: MusicBrainz release search, then the Cover Art Archive front image. Requests carry a proper
   `User-Agent` and respect MusicBrainz's 1 request/second limit.
5. Otherwise, `presence.default_art_asset`.

Matching is fuzzy but careful. Case and punctuation are normalized, and noise such as `(feat. …)`, `- Remastered 2011`,
`[Explicit]` and `(Deluxe Edition)` is stripped. A result is rejected unless the artist matches and the album/title
is similar.

Lookups are **asynchronous** (5 s timeout per request) and never block the presence. The card is shown immediately
with the default art, and the image is swapped in once found. That swap is a normal rate-limited update.

Results are cached in memory and in `~/.cache/tunecord/art.json`. The key is the normalized artist + album, or
artist + title when there's no album. Positive results are kept for 30 days. "Not found" results are kept for
24 hours, and network failures for 15 minutes, so the APIs aren't hammered. Delete the file to clear the cache.
URLs longer than 256 characters (Discord's limit) are rejected.

### Spotify setup (optional)

1. Go to <https://developer.spotify.com/dashboard> and create an app. Any name works. Use any redirect URI, e.g.
   `http://127.0.0.1/callback`; it's not used. Tick "Web API".
2. Open the app's settings and copy the **Client ID** and **Client Secret**.
3. Run `tunecord config`, keep `spotify` in the art sources and paste both values. The secret input is masked.

The credentials are stored in your config (mode `0600`) and never logged. Without them, the Spotify source is
skipped automatically.

## Discord: Flatpak, Snap, Vesktop & co.

tunecord connects to Discord's local IPC socket and searches, in order:

- `$XDG_RUNTIME_DIR/discord-ipc-{0..9}` (regular Discord, PTB, Canary)
- `$XDG_RUNTIME_DIR/app/com.discordapp.Discord/…` and `…/.flatpak/com.discordapp.Discord/xdg-run/…` (Flatpak)
- `$XDG_RUNTIME_DIR/snap.discord/…`, `snap.discord-canary/…` (Snap)
- `app/dev.vencord.Vesktop/…` (Vesktop Flatpak), `app/xyz.armcord.ArmCord/…`, `app/app.legcord.Legcord/…`,
  `app/io.github.spacingbat3.webcord/…`
- the same under `/run/user/<uid>`, then `$TMPDIR` and `/tmp`

If Discord isn't running, tunecord keeps retrying with exponential backoff (1 s up to 30 s) and picks it up as soon
as Discord starts. Discord restarts, socket EOFs and write errors cause a reconnect, never a crash. The current
presence is re-sent after reconnecting.

**Flatpak Discord:** recent Flatpak builds expose the socket under `app/com.discordapp.Discord`, which tunecord
finds automatically. If yours doesn't, link it once:
`ln -sf $XDG_RUNTIME_DIR/app/com.discordapp.Discord/discord-ipc-0 $XDG_RUNTIME_DIR/discord-ipc-0`.

**Snap Discord:** the socket lives under `$XDG_RUNTIME_DIR/snap.discord/` and is found automatically.

**Vesktop/Vencord, ArmCord/Legcord, WebCord:** these need their "arRPC"/Rich Presence option enabled.

In Discord itself, *Settings → Activity Privacy → "Share your detected activities with others"* must be on.

## Troubleshooting

| Symptom | What to check |
|---|---|
| Nothing appears in Discord | `tunecord status`. Is the allowlist empty? Is the player allowlisted (compare with `tunecord list`)? Is Discord connected? Is activity sharing enabled in Discord's privacy settings? |
| `cannot connect to the D-Bus session bus` | tunecord must run inside your desktop session. With systemd this works out of the box. Over SSH/TTY there's no session bus; export `DBUS_SESSION_BUS_ADDRESS` from your session if you really need it. |
| Discord shows "not connected" | Discord not running, or a client/sandbox whose socket isn't exposed (see above). Logs: `journalctl --user -u tunecord -f`. |
| Wrong player is shown | Reorder the allowlist (`tunecord config`): higher entries win. `tunecord list` shows which one is active. |
| Progress bar missing or wrong | `tunecord debug --once`. Look at the verdict and the raw types. A stream (no length) never has a bar. With `fallback = "skip"`, unreliable players get no bar. |
| Bar jumps after a seek with a few seconds delay | Expected: Discord updates are spaced at least 4 s apart to respect its rate limit. |
| Album art is the default icon | `tunecord status` shows the art in use. With `-v` the service logs lookups. Obscure releases may simply not be in iTunes/MusicBrainz; try Spotify credentials. |
| Config changes not applied | The service only reads the config at startup. Use `tunecord config`, or restart the unit after editing by hand. |
| `tunecord: command not found` | `~/.local/bin` isn't on your `PATH` (see [Install](#install)). The service itself doesn't care. |
| Service won't start after moving the binary | Re-run `tunecord install --user` to rewrite `ExecStart`. |

Useful commands:

```sh
systemctl --user status tunecord
journalctl --user -u tunecord -f          # logs
tunecord run -v                           # foreground with debug logging (stop the unit first)
```

## Design decisions

These are places where the spec was ambiguous, and the choice made:

- **Timestamps are sent in Unix milliseconds.** Discord's Rich Presence documents `timestamps.start/end` in
  milliseconds. Second-resolution values are interpreted as 1970 by some clients and drift by up to a second, so
  tunecord sends ms. They are still derived from the anchor model, so they don't jitter.
- **Extra option `presence.paused_asset`.** The paused marker is " · Paused" appended to the state line by default.
  If you upload an asset (or give an https URL), it's shown as the small image instead, and the state line stays
  clean.
- **Internal timing constants** aren't configurable, to keep the config small: 4 s minimum push spacing / 5 per 20 s,
  400 ms track-change settle delay, 3 s (and 3 samples) stuck-position grace, 2 s overshoot tolerance, 24 h maximum
  length, and a 60 s full resync as a safety net against missed signals.
- **Track identity** uses `mpris:trackid` + title + artists. Album and length aren't part of it, so players that fill
  those in late don't reset the bar.
- **`list --active`** prints tunecord's own choice, and falls back to the first playing player when no allowlisted
  player qualifies. It exits 1 when there is nothing.
- **`debug --once`** observes for about 3 s before printing, because a single sample can't tell a stuck position
  from a sane one.
- **Single instance.** `tunecord run` refuses to start if the pidfile belongs to another live tunecord process.
  Stale pidfiles are replaced.
- **`install` without `--user`** exits with status 2 and an explanation. A system service can't reach your
  session bus or Discord's socket.
- **Repository URL.** The MusicBrainz `User-Agent` and the unit's `Documentation=` point at the actual repository,
  `https://github.com/cmerk2021/tunecore`. The binary, module, config directory and unit are all named `tunecord`.
- **Go version.** `go.mod` requires the Go version needed by the current `golang.org/x/text`. Use the latest stable Go.

## Building from source

```sh
git clone https://github.com/cmerk2021/tunecore && cd tunecore
CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/tunecord ./cmd/tunecord
go test ./...                      # the D-Bus integration test runs if dbus-daemon is installed
goreleaser release --snapshot --clean   # local release build into dist/
```

Project layout:

```
cmd/tunecord/          entry point
internal/cli/          cobra commands
internal/config/       load/validate/save, defaults, XDG paths
internal/mpris/        D-Bus client, discovery, signals, type normalization
internal/progress/     anchor model, seek/rate/stuck detection, fallback estimation
internal/presence/     Discord activity builder (truncation, padding, paused, status_display)
internal/discord/      IPC client, reconnect with backoff, rate limiting/coalescing
internal/art/          resolver chain, Spotify, iTunes, MusicBrainz, cache
internal/service/      run loop, player selection, reload handling, status file
internal/control/      pidfile, systemctl wrapper, reload-on-save
internal/tui/          huh forms + Bubble Tea reorder list
internal/install/      systemd unit generation, install/uninstall
packaging/systemd/     reference unit file
```

## License

MIT, see [LICENSE](LICENSE).
