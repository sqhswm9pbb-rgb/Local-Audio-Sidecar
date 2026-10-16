# Local Audio Sidecar

A small LAN audio server for STR. It scans folders on a computer or NAS, lists
folders and individual tracks as channels, and streams MP3 audio to a Bose
SoundTouch speaker. Named channels can combine tracks from several folders with
shuffle and repeat settings.

## Quick start

1. Install Go 1.22 or newer and FFmpeg. Make sure `ffmpeg` is available on
   `PATH`.
2. Copy `local-audio.example.json` to `music.json` and replace the sample
   paths with folders and files on the computer running this service.
3. Build and run it:

   ```powershell
   .\build.ps1
   .\local-audio-sidecar.exe
   ```

   Or build from another operating system:

   ```sh
   ./build.sh
   ./local-audio-sidecar
   ```

   You can pass another config file as the first argument:

   ```powershell
   .\local-audio-sidecar.exe D:\STR\music.json
   ```

The server listens on `0.0.0.0:8092` by default. It prints a LAN base URL, for
example `http://192.168.1.25:8092`. In STR, open **Stream source**, enter that
base URL, and browse the channels. STR and the speaker must be able to reach
the sidecar host over the LAN.

## Command-line help and quick modes

Run `local-audio-sidecar.exe --help` to see all options. Flags can appear
before or after the folder path.

Generate `music.json` in the current directory by scanning a folder:

```powershell
.\local-audio-sidecar.exe C:\Music --generate
```

The generated file stores the folder path; the sidecar discovers its folders
and individual files when it starts. Existing `music.json` files are preserved
unless you explicitly pass `--force`. Use `--output FILE` to choose another
destination.

Play a folder temporarily without reading or writing any JSON files:

```powershell
.\local-audio-sidecar.exe C:\Music --temp --repeat --watch --port 8095
```

Pass multiple folders to keep their regular channels and add a combined mix
channel. The mix channel appears alongside each folder, subfolder, and
individual-track channel:

```powershell
.\local-audio-sidecar.exe C:\Music "C:\Users\jpodl\Rolling Stones" --shuffle --mix --watch
```

`--mix` requires at least two folder paths. It starts temporary playback, so no
JSON file is needed. `--shuffle` also shuffles the combined channel. The normal
folder and file channels remain in the station list.

Temporary mode does not repeat by default. `--repeat` loops each channel, and
`--shuffle` randomizes track order. These switches also override the playback
settings of channels loaded from a config file. The default port is 8092; a
`--port` value overrides it for that run.

Add `--watch` to rescan the configured folders every 30 seconds. Set a custom
interval with `--watch-interval 1m` (minimum 5 seconds). New tracks and folders
appear in the station list, and removed tracks are dropped. An active repeating
folder stream picks up the refreshed track list on its next cycle; one-pass
streams use it on their next connection.

## Library and channels

`music_dirs` lists folders to scan. Each configured folder and its subfolders
appear as channels. Each folder channel includes audio below it recursively.
Every supported audio file also appears as its own channel. File channels
repeat the file so they behave like a continuous station.

Automatically generated channel IDs use the folder or file name, with spaces
replaced by underscores (for example, `The Doors` becomes `The_Doors`). File
channel IDs include the file extension. If two channels would have the same ID,
the duplicate receives a short suffix to keep both channels addressable.

Use `channels` to create a named mix. A custom channel can combine any number
of folders and individual files. Paths can be absolute or relative to the
config file, and folder contents are scanned recursively.

```json
{
  "music_dirs": ["C:/Users/YourName/Music"],
  "channels": [
    {
      "id": "weekend-mix",
      "name": "Weekend Mix",
      "folders": [
        "C:/Users/YourName/Music/Jazz",
        "D:/Audio/Soul"
      ],
      "files": ["C:/Users/YourName/Music/Theme.mp3"],
      "shuffle": true,
      "repeat": "all"
    }
  ]
}
```

Repeat modes:

- `off`: play each item once, then end the stream.
- `all`: replay the channel from the beginning after its last item.
- `one`: keep replaying the selected item. With shuffle enabled, one item is
  selected randomly when playback starts.

Custom channels default to `repeat: "all"` and `shuffle: false`. Add a
top-level `defaults` setting to control automatically discovered folder and
file channels together:

```json
"defaults": { "shuffle": true, "repeat": "all" }
```

Changes to the config take effect after restarting the sidecar.

Supported extensions include MP3, M4A, AAC, FLAC, WAV, OGG, Opus, WMA, AIFF,
and ALAC. FFmpeg decodes each track and streams stereo MP3 at 256 kbps, so the
speaker can play formats it does not decode natively. One MP3 encoder remains
open across track changes, and local files are read at playback speed. No audio
is written to disk. The sidecar reads embedded title, artist, album, and cover art when
`ffprobe` is available alongside FFmpeg or on `PATH`; otherwise it falls back
to filenames. A filename in `Artist - Title.ext` form supplies the artist and
title fallback. Embedded cover images are served as scaled JPEGs through the
artwork URL returned by the now-playing endpoint.

## API (same format as SiriusXM Sidecar)

Set STR's stream-source base URL to the server root. The endpoints are:

- `GET /api/health` returns `{"status":"ok"}`.
- `GET /api/stations` returns `[{"id":"...","name":"..."}]`.
- `GET /api/stations/{id}` returns the station name and stream URL.
- `GET /api/stations/{id}/now-playing` returns current title and artist.
- `GET /api/tracks/{id}/art` returns embedded cover art for the current track.
- `GET /api/stations/{id}/stream` streams `audio/mpeg` continuously.
- `GET /api/stations/{id}/stream?skip=1` skips the current track in active
  streams for that channel and returns `202 Accepted`.

To skip from a browser or another HTTP client, copy the channel's stream URL
and add `?skip=1`. For example:

```text
http://192.168.1.25:8092/api/stations/weekend-mix/stream?skip=1
```

The response reports how many active streams received the skip request. If
the channel is not currently streaming, the endpoint returns `409 Conflict`.
For a channel containing one repeated file, skip restarts that same file.

Example station response:

```json
{
  "id": "weekend-mix",
  "name": "Weekend Mix",
  "stream_url": "http://192.168.1.25:8092/api/stations/weekend-mix/stream",
  "content_type": "audio/mpeg"
}
```

## Settings

- `LOCAL_AUDIO_CONFIG`: config path used when no command-line path is provided.
- `SIDECAR_HOST`: listen address; defaults to `0.0.0.0`.
- `SIDECAR_PORT`: listen port; defaults to `8092`.
- `SIDECAR_BASE_URL`: public LAN URL returned to clients; otherwise the
  service detects a LAN IPv4 address.
- `FFMPEG`: FFmpeg executable path; defaults to `ffmpeg`.
- `FFPROBE`: optional FFprobe executable path for embedded tags and cover art;
  otherwise the sidecar checks beside FFmpeg and then on `PATH`.

If the computer has multiple network adapters, set `SIDECAR_BASE_URL` to the
address reachable by the speaker. Allow inbound TCP on the selected port in the
computer's firewall.
