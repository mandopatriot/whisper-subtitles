# Whisper Subtitles

A [HolyArrs](https://github.com/HolyArrs/HolyArrs) plugin that writes
subtitles from the video's audio and lines up saved subtitles with the
speech, using your own [Whisper Pro ASR](https://github.com/ventura8/Whisper-Pro-ASR)
server.

It does two jobs with one Whisper server, and only one piece of audio goes
to the server at a time. Writing a subtitle goes before syncing one.

## Subtitles written from the audio

Add a subtitle site of kind **Plugin** with the address
`plugin://whisper-subtitles`. HolyArrs then searches it like any other site.

- **What is offered.** One subtitle for each wanted language spoken in one of
  the video's audio tracks. Only enabled tracks tagged with a language count;
  untagged tracks, commentary and audio description are never offered.
  Searching reads only the video's header and never calls Whisper.
- **How it is written.** When HolyArrs picks an offer, the plugin sends the
  track to Whisper in 90-second pieces (with 15 seconds more on each side, so
  speech across a cut is heard whole), always with the track's language. The
  smallest track in that language is used, so a lossy track goes before a
  lossless one. Pieces stop at 64 MiB, so a lossless track is sent in shorter
  pieces. The SRT is built from Whisper's timed segments: at most two lines
  of 42 characters and 7 seconds per line, split at word timings when the
  server gives them.
- **What is left out.** Segments Whisper marks as likely not speech: a
  `no_speech_prob` above 0.6 together with an `avg_logprob` below -1.0
  (Whisper's own silence rule), or a `no_speech_prob` above 0.9 on its own.
  Low confidence alone never removes a line, since it can be real speech that
  is hard to hear. Lines of only punctuation, such as ".". A line that is
  only "you", "thank", "thank you", "thanks for watching" or "thank you for
  watching" and starts within 250 ms of a piece's start or a 30-second mark
  inside it, where Whisper makes them up (the same words anywhere else are
  kept). A line Whisper repeats three or more times in a row is
  kept once. A video over 20 minutes with fewer than 10 lines heard gets no
  subtitle.
- **What HolyArrs does with it.** It marks the file *Written from the audio*,
  uses it only when no subtitle made by a person passes, and replaces it when
  a person's subtitle turns up later. The plugin never writes into your
  Library.
- **Restarts.** Each finished piece and each finished subtitle is kept in the
  plugin's data folder (subtitles for 7 days, at most 200), so a restart of
  the plugin or of HolyArrs picks up where it stopped.

## Sync

When HolyArrs saves or upgrades a subtitle, the plugin cuts twelve 90 second
pieces of the video's sound (away from the end credits), asks your Whisper
server what is said and when, and matches that against the subtitle's lines.
If the subtitle is early, late, or made for another frame rate, and the
pieces agree, it uses HolyArrs's own Sync fix. The old file goes to the
holding folder and History shows the change. If it is not sure, it leaves
the subtitle alone and logs why.

Waiting syncs are kept in the plugin's data folder, so a restart picks them
up again; one cut off by the restart runs again from its start. A sync
stopped by the time limit or by the Whisper server not answering in time is
tried again by itself, after 30 minutes and then after 2 hours, and then
given up. A subtitle that was checked and left alone is not tried again.

### Timing: words or phrases

- **Word timings give precise sync.** Whisper Pro ASR engines based on
  Faster-Whisper return a time for every word, and the plugin then fixes
  offsets from 0.3 seconds up.
- **Phrase timings are rougher.** Whisper Pro ASR's INTEL-WHISPER engine
  returns phrase timings only unless `INTEL_WORD_TIMESTAMPS=true` is set,
  and in testing those were off by up to half a second per file. With phrase
  timings, the plugin moves a subtitle only when it is off by 1 second or
  more, so a subtitle that is already right is never moved by the server's
  own error. Subtitles written from phrase timings can be up to half a second
  off too.

The plugin asks for word timings on every call and uses them whenever the
server sends them.

### What it checks

- Only subtitles HolyArrs saved (fetched or chosen by hand), beside the
  video, in SRT, VTT, ASS or SSA, never synced before, and not written from
  the audio.
- Shows scoring at or above 96, and movies at or above 86, are trusted and
  skipped (both can be changed).
- Subtitles in another language than the sound are skipped.
- The audio track in the subtitle's language is used, a lossy one before a
  lossless one, leaving out commentary and audio description tracks. Its
  language is sent to the server.

## When the Whisper server is away

If the server can't be reached, nothing fails. That is a refused
connection or a name that doesn't resolve, or a lost connection, a 403 or
a 502, 503 or 504 while `GET /asr` fails too. (Through HolyArrs's proxy a
stopped server shows up as an empty 403 or a reset connection, never as
refused.) Then uploads stop, the plugin tries `GET /asr` every 30 seconds,
and both writing and syncing carry on as soon as it answers. The log says
once when it went away and once when it is back. A subtitle being written
keeps its finished pieces, and fails only if the server is still away when
HolyArrs stops waiting for it (an hour after it asked). A server that took
an upload and never answered is different, and so is a 502, 503 or 504
while `GET /asr` answers at once (the server is up but busy): uploads then
pause for 5 minutes, doubling up to an hour. A subtitle being written
waits out the pause and goes on, keeping its pieces; it fails only if the
pause would run past HolyArrs's hour. A sync stops and is tried again
later, and so is one that lost windows to the connection and then had too
few windows to decide: it is not counted as left alone.

## Setup

1. Approve the Library folders that hold your movies and shows (read only).
2. Set the Whisper server address, such as `http://whisper.example.lan:9000`,
   and add the same name and port under **Also allow your own server**.
3. For subtitles written from the audio, add a subtitle site of kind
   **Plugin** with the address `plugin://whisper-subtitles`. Its
   **Downloads per day** caps how many it writes.
4. Turn on **Check only** for a first run of sync: it logs what it would
   change and changes nothing.
5. To check titles by hand, list their ids in **Check these titles** and save.

**Write subtitles from the audio** and **Line up saved subtitles** turn each
job off on its own.

MKV and WebM files are read today; other containers get no offer and are
skipped by sync with a log line.

## Licence

AGPL-3.0
