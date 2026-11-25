# Subtitle Sync

A [HolyArrs](https://github.com/HolyArrs/HolyArrs) plugin that lines up saved
subtitles with the speech in the video, using your own
[Whisper Pro ASR](https://github.com/ventura8/Whisper-Pro-ASR) server.

When HolyArrs saves or upgrades a subtitle, subtitle-sync cuts twelve 90 second
pieces of the video's sound (away from the end credits), asks your Whisper
server what is said and when, and matches that against the subtitle's lines.
If the subtitle is early, late, or made for another frame rate, and the
pieces agree, it uses HolyArrs's own Sync fix. The old file goes to the
holding folder and History shows the change. If it is not sure, it leaves
the subtitle alone and logs why.

## Timing: words or phrases

- **Word timings give precise sync.** Whisper Pro ASR engines based on
  Faster-Whisper return a time for every word, and subtitle-sync then fixes
  offsets from 0.3 seconds up.
- **Phrase timings are rougher.** Whisper Pro ASR's INTEL-WHISPER engine
  currently returns phrase timings only, and in testing those were off by up
  to half a second per file. With phrase timings, subtitle-sync moves a subtitle
  only when it is off by 1 second or more, so a subtitle that is already
  right is never moved by the server's own error.

subtitle-sync asks for word timings on every call and uses them whenever the
server sends them.

## What it checks

- Only subtitles HolyArrs saved (fetched or chosen by hand), beside the
  video, in SRT, VTT, ASS or SSA, never synced before.
- Shows scoring at or above 96, and movies at or above 86, are trusted and
  skipped (both can be changed).
- Subtitles in another language than the sound are skipped.
- The audio track in the subtitle's language is used, a lossy one before a
  lossless one, leaving out commentary and audio description tracks. Its
  language is sent to the server.

## Setup

1. Approve the Library folders that hold your movies and shows (read only).
2. Set the Whisper server address, such as `http://whisper.example.lan:9000`,
   and add the same name and port as one of your own hosts for this plugin.
3. Turn on **Check only** for a first run: it logs what it would change and
   changes nothing.
4. To check titles by hand, list their ids in **Check these titles** and save.

MKV and WebM files are read today; other containers are skipped with a log
line.

## Licence

AGPL-3.0
