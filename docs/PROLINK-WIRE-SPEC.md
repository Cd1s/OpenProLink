# Pro DJ Link wire spec (as implemented)

What `cmd/cdj3000-sim` sends and parses, and what `pkg/cdj` implements. Offsets are hexadecimal, integers big-endian unless specified otherwise.
References:
- Deep Symmetry protocol analysis (<https://djl-analysis.deepsymmetry.org/>) and captures (`dysentery`, `beat-link`, `crate-digger`)
- `pyrekordbox` binary format specifications (<https://github.com/dylanljones/pyrekordbox>)
- Local hardware captures in `pkg/prolink/testdata/captured-wire.json` (CDJ-2000nexus firmware 1.44)

Fields are explicitly categorized into:
- **[Confirmed]**: Verified against Deep Symmetry hardware captures (S01–S20) and/or `captured-wire.json`.
- **[CDJ-3000 Extension]**: Reverse-engineered on CDJ-3000 firmware, awaiting additional multi-deck live hardware captures.

---

## 1. Common Header

Every Pro DJ Link packet starts with a 10-byte magic header, followed by a packet type byte and device name.

| Offset | Size | Value / Meaning | Status |
| --- | --- | --- | --- |
| `0x00`–`0x09` | 10 | ASCII `Qspt1WmJOL` (`0x51 0x73 0x70 0x74 0x31 0x57 0x6d 0x4a 0x4f 0x4c`) | **[Confirmed]** |
| `0x0a` | 1 | Packet type (`0x06` keepalive/media resp, `0x05` media query, `0x0a` status, `0x28` beat) | **[Confirmed]** |
| `0x0b` or `0x0c` | 20 | Device name, NUL-padded ASCII (e.g. `CDJ-3000` or `CDJ-2000nexus`). Offset `0x0b` on UDP 50001/50002; offset `0x0c` on UDP 50000 (which has an extra subtype byte `0x00` at `0x0b`) | **[Confirmed]** |
| name + 20 | 1 | Header terminator `0x01` (at offset `0x1f` for 50001/50002, or `0x20` for 50000) | **[Confirmed]** |

---

## 2. UDP 50000 Keepalive (Type `0x06`, 54 bytes / `0x36`)

Broadcast to UDP port 50000 every 1.5–2.0 s (2.0 s tight timing on settled CDJs), and immediately upon player number change.

| Offset | Size | Value / Meaning | Status |
| --- | --- | --- | --- |
| `0x00`–`0x09` | 10 | `Qspt1WmJOL` | **[Confirmed]** |
| `0x0a` | 1 | Packet type `0x06` | **[Confirmed]** |
| `0x0b` | 1 | Subtype `0x00` | **[Confirmed]** |
| `0x0c`–`0x1f` | 20 | Device name ASCII, NUL-padded (`CDJ-3000`) | **[Confirmed]** |
| `0x20` | 1 | `0x01` | **[Confirmed]** |
| `0x21` | 1 | Packet subtype `0x02` | **[Confirmed]** |
| `0x22`–`0x23` | 2 | Total packet length: `0x0036` (54 bytes) | **[Confirmed]** |
| `0x24` | 1 | Player / Device number $D$ (1–4 standard, 1–6 on CDJ-3000) | **[Confirmed]** |
| `0x25` | 1 | First-device flag: `0x02` if first device on network at boot, `0x01` if joined existing network (latched at startup) | **[Confirmed]** |
| `0x26`–`0x2b` | 6 | MAC address (6 bytes). Real players keep MAC invariant across player number changes | **[Confirmed]** |
| `0x2c`–`0x2f` | 4 | IPv4 address of the player | **[Confirmed]** |
| `0x30` | 1 | Peer count $p$: number of devices seen on network including itself (starts at 1) | **[Confirmed]** |
| `0x31`–`0x33` | 3 | Padding `0x00 0x00 0x00` | **[Confirmed]** |
| `0x34` | 1 | Device class: `0x01` = CDJ player, `0x02` = DJM mixer | **[Confirmed]** |
| `0x35` | 1 | Model capability flag: `0x00` on CDJ-2000nexus; `0x64` (100) on CDJ-3000 compatible players (omitting `0x64` causes CDJ-3000 players 5 & 6 to drop off) | **[Confirmed]** |

---

## 3. UDP 50002 CDJ Status (Type `0x0a`)

Unicast (or broadcast) to port 50002 roughly every 200 ms.
Packet lengths:
- Pre-nexus: 208 bytes (`0xd0`)
- CDJ-2000nexus: 212 bytes (`0xd4`)
- CDJ-2000NXS2: 284 bytes (`0x11c`) or 292 bytes (`0x124`)
- CDJ-3000: 512 bytes (`0x200`)
The length field at offset `0x22` holds remaining payload size: `size - 0x24`.

| Offset | Size | Name | Value / Meaning | Status |
| --- | --- | --- | --- | --- |
| `0x00`–`0x09` | 10 | Magic | `Qspt1WmJOL` | **[Confirmed]** |
| `0x0a` | 1 | Type | `0x0a` | **[Confirmed]** |
| `0x0b`–`0x1e` | 20 | Name | `CDJ-3000` (NUL-padded) | **[Confirmed]** |
| `0x1f` | 1 | Terminator | `0x01` | **[Confirmed]** |
| `0x20` | 1 | Subtype | Packet protocol version: `0x03`, `0x04`, `0x05`, `0x06` seen on CDJs (`0x00` on mixers) | **[Confirmed]** |
| `0x21` | 1 | Player $D$ | Player number (1–6) | **[Confirmed]** |
| `0x22`–`0x23` | 2 | Length | Payload length = `total_size - 0x24` (`0x00f8` for 0x11c, `0x01dc` for 0x200) | **[Confirmed]** |
| `0x24` | 1 | Player $D$ | Player number (repeated) | **[Confirmed]** |
| `0x25` | 1 | Subtype 2 | `0x00` | **[Confirmed]** |
| `0x26` | 1 | Subtype 3 | `0x01` | **[Confirmed]** |
| `0x27` | 1 | Activity $A$ | `0x00` idle, `0x01` playing / searching / loading | **[Confirmed]** |
| `0x28` | 1 | Track player $D_r$ | Device number where track was loaded from (`0x00` if no track) | **[Confirmed]** |
| `0x29` | 1 | Track slot $S_r$ | `0x00` no track, `0x01` CD, `0x02` SD, `0x03` USB, `0x04` rekordbox collection, `0x06` Streaming Direct Play, `0x07` USB 2, `0x09` Beatport LINK (CDJ-3000), `0x0e` Beatport LINK (XDJ-AZ) | **[Confirmed]** |
| `0x2a` | 1 | Track type $T_r$ | `0x00` no track, `0x01` rekordbox analyzed, `0x02` unanalyzed, `0x05` CD audio, `0x06` streaming | **[Confirmed]** |
| `0x2b` | 1 | Reserved | `0x00` | **[Confirmed]** |
| `0x2c`–`0x2f` | 4 | Track ID | rekordbox track ID (uint32 big-endian, `0` if none) | **[Confirmed]** |
| `0x32`–`0x33` | 2 | Track index | Track number within current playlist / browse list | **[Confirmed]** |
| `0x35` | 1 | $t_{srt}$ | Browse sort mode: `0x00` default, `0x01` title, `0x02` artist, `0x03` album, `0x04` BPM, `0x05` rating, `0x0c` key | **[Confirmed]** |
| `0x37` | 1 | $t_{src}$ | Menu source: `0x00` none, `0x02` artist, `0x03` album, `0x04` track, `0x05` playlist, `0x06` BPM, `0x0c` key, `0x11` folder, `0x12` search song, `0x16` history, `0x28` tag list, `0x32` instant double | **[Confirmed]** |
| `0x38`–`0x3f` | 8 | $t_{cat1}, t_{cat2}$ | Menu selection hierarchy category IDs | **[Confirmed]** |
| `0x46`–`0x47` | 2 | $d_n$ | Number of tracks in disc / playlist / menu | **[Confirmed]** |
| `0x58` | 1 | $ld_1$ | Brief load trigger: `0x80` on track load start (NXS2/XDJ) | **[Confirmed]** |
| `0x5a`–`0x5b` | 2 | $u_{c1}$ | Cue update trigger: `0xffff` for 1 packet when memory/hot cue added/removed | **[Confirmed]** |
| `0x5e`–`0x5f` | 2 | $u_t$ | Tag list trigger: `0x00ff` -> `0xff00` -> `0x0000` | **[Confirmed]** |
| `0x66`–`0x67` | 2 | $ld_2$ | Track load complete trigger: `0xffff` for 1 packet (NXS2/XDJ) | **[Confirmed]** |
| `0x6a` | 1 | $U_a$ | USB activity LED indicator (alternates `0x04` and `0x06` during access) | **[Confirmed]** |
| `0x6b` | 1 | $S_a$ | SD activity LED indicator (alternates `0x04` and `0x06` during access) | **[Confirmed]** |
| `0x6f` | 1 | $U_l$ | USB local state: `0x00` loaded/mounted, `0x02`/`0x03` unmounting/stop pressed, `0x04` empty/no media | **[Confirmed]** |
| `0x73` | 1 | $S_l$ | SD local state: `0x00` loaded/mounted, `0x02`/`0x03` unmounting/door open, `0x04` empty/no media | **[Confirmed]** |
| `0x75` | 1 | $L$ | Link media available: `0x01` if USB/SD/CD present on any link player, `0x00` otherwise | **[Confirmed]** |
| `0x7b` | 1 | $P_1$ | Primary play state: `0x00` no track, `0x02` loading, `0x03` playing, `0x04` looping, `0x05` paused, `0x06` paused at cue, `0x07` cue play, `0x08` cue scratch, `0x09` searching, `0x0e` CD sleep, `0x11` end of track, `0x12` emergency loop | **[Confirmed]** |
| `0x7c`–`0x7f` | 4 | Firmware | ASCII firmware string, e.g. `3.20` or `1.44` | **[Confirmed]** |
| `0x84`–`0x87` | 4 | $Sync_n$ | Master takeover counter, incremented when yielding tempo master | **[Confirmed]** |
| `0x89` | 1 | Flags $F$ | Bit 6 (`0x40`) Play, Bit 5 (`0x20`) Master, Bit 4 (`0x10`) Sync, Bit 3 (`0x08`) On-Air, Bit 1 (`0x02`) BPM Sync | **[Confirmed]** |
| `0x8b` | 1 | $P_2$ | Play state 2: pre-nexus `6a`/`6e`, nexus `7a`/`7e`, NXS2 `fa`/`fe`, XDJ-XZ `9a`/`9e` (low nibble: `a` moving, `e` stopped) | **[Confirmed]** |
| `0x8c`–`0x8f` | 4 | $Pitch_1$ | Effective playback pitch: $0x100000 \times (1 + pct / 100)$, $0x100000$ = 0% | **[Confirmed]** |
| `0x90`–`0x91` | 2 | $M_v$ | Master BPM validity: `0x8000` valid rekordbox BPM, `0x7fff` no track, `0x0000` unanalyzed | **[Confirmed]** |
| `0x92`–`0x93` | 2 | BPM | Track BPM $\times 100$ (`0xffff` if no track) | **[Confirmed]** |
| `0x94`–`0x95` | 2 | $M_{slip}$ | Slip BPM validity (`0x7fff` when not slipping) | **[Confirmed]** |
| `0x96`–`0x97` | 2 | $BPM_{slip}$ | Slip track BPM $\times 100$ (`0xffff` when not slipping) | **[Confirmed]** |
| `0x98`–`0x9b` | 4 | $Pitch_2$ | Fader pitch adjustment (tracks physical pitch slider) | **[Confirmed]** |
| `0x9d` | 1 | $P_3$ | Play state 3: `0x00` no track, `0x01` paused/reverse, `0x09` forward vinyl, `0x0b` slipping, `0x0d` forward CDJ | **[Confirmed]** |
| `0x9e` | 1 | $M_m$ | Master meaningful: `0x00` not master, `0x01` master with rekordbox, `0x02` master with unanalyzed | **[Confirmed]** |
| `0x9f` | 1 | $M_h$ | Master handoff: `0xff` normally; incoming master device number during handoff | **[Confirmed]** |
| `0xa0`–`0xa3` | 4 | Beat | 1-based beat counter in track (`0` paused at start, `0xffffffff` if no track) | **[Confirmed]** |
| `0xa4`–`0xa5` | 2 | Cue bar | Countdown to next cue: `0x01ff` if $>64$ bars, down to `0x0000` at cue | **[Confirmed]** |
| `0xa6` | 1 | $B_b$ | Beat within bar (1–4, `0` if unanalyzed) | **[Confirmed]** |
| `0xb3` | 1 | $u_g$ | Beat grid edited trigger (`0xff` for 1 packet) | **[Confirmed]** |
| `0xb7` | 1 | $M_p$ | CDJ-3000 Media Presence bitmask: bit 0 (`0x01`) SD present, bit 1 (`0x02`) USB present (`0x00` on legacy) | **[CDJ-3000 Extension]** |
| `0xb8` | 1 | $U_e$ | USB unsafe eject flag (`1` if removed unsafely, `0` normal) | **[Confirmed]** |
| `0xb9` | 1 | $S_e$ | SD unsafe eject flag (`1` if removed unsafely, `0` normal) | **[Confirmed]** |
| `0xba` | 1 | $el$ | Emergency loop / emergency mode active (`1` active, `0` normal) | **[Confirmed]** |
| `0xc0`–`0xc3` | 4 | $Pitch_3$ | Effective pitch copy 3 | **[Confirmed]** |
| `0xc4`–`0xc7` | 4 | $Pitch_4$ | Fader pitch copy 4 | **[Confirmed]** |
| `0xc8`–`0xcb` | 4 | Packet | 4-byte packet counter (fixed `00 00 00 00` on CDJ-3000) | **[Confirmed]** |
| `0xcc` | 1 | $nx$ | Hardware generation: `0x05` pre-nexus, `0x0f` nexus, `0x1f` CDJ-3000 / XDJ-XZ | **[Confirmed]** |
| `0xcd` | 1 | Touch | Touch Audio support flag (bit 5 = 1) | **[Confirmed]** |
| `0xd0`–`0xef` | 32 | Settings 1 | Settings block: starts with `12 34 56 78`, byte `0xda` = waveform color (`0x01` Blue, `0x03` RGB, `0x04` 3-Band) | **[Confirmed]** |
| `0xf0`–`0x10f` | 32 | Settings 2 | Settings block 2 (CDJ-3000 only) | **[CDJ-3000 Extension]** |
| `0x113` | 1 | $P_4$ | CDJ-3000 playback bitmask | **[CDJ-3000 Extension]** |
| `0x116`–`0x117` | 2 | $T_b$ | Time steps per bar for micro-phase meter | **[CDJ-3000 Extension]** |
| `0x11a`–`0x11b` | 2 | $T_{pos}$ | Time position in current bar | **[CDJ-3000 Extension]** |
| `0x11c` | 1 | Next Cue | Upcoming memory point index (`0x0a` first, `0x0b` second, `0x00` none) | **[CDJ-3000 Extension]** |
| `0x11d`–`0x11e` | 2 | Buffer | Forward and backward track RAM cache length | **[CDJ-3000 Extension]** |
| `0x11f` | 1 | Buffer Full | `0x01` when complete track is buffered in RAM | **[CDJ-3000 Extension]** |
| `0x120`–`0x123` | 4 | Needle Drag | Timestamp of touchscreen needle search marker (ms) | **[CDJ-3000 Extension]** |
| `0x158` | 1 | Master Tempo | Master Tempo engaged (`0x00` off, `0x01` on) | **[CDJ-3000 Extension]** |
| `0x15c`–`0x15e` | 3 | Key | Musical key: `0x15c` note (0–11), `0x15d` mode (0 minor, 1 major), `0x15e` accidental (0 natural, 1 sharp, ff flat) | **[CDJ-3000 Extension]** |
| `0x164`–`0x16b` | 8 | KeyShift | Signed 64-bit key shift in cents (100 cents = 1 semitone) | **[CDJ-3000 Extension]** |
| `0x1b6`–`0x1b9` | 4 | Loop Start | Active loop start ms: $val \times 65536 / 1000$ | **[CDJ-3000 Extension]** |
| `0x1be`–`0x1c1` | 4 | Loop End | Active loop end ms: $val \times 65536 / 1000$ | **[CDJ-3000 Extension]** |
| `0x1c8`–`0x1c9` | 2 | Loop Beats | Number of whole beats in active loop | **[CDJ-3000 Extension]** |

---

## 4. UDP 50001 Beat (Type `0x28`, 96 bytes / `0x60`)

Broadcast on UDP 50001 on each beat while playing.

| Offset | Size | Value / Meaning | Status |
| --- | --- | --- | --- |
| `0x00`–`0x09` | 10 | `Qspt1WmJOL` | **[Confirmed]** |
| `0x0a` | 1 | Packet type `0x28` | **[Confirmed]** |
| `0x0b`–`0x1e` | 20 | Device name (`CDJ-3000`, NUL-padded) | **[Confirmed]** |
| `0x1f` | 1 | `0x01` | **[Confirmed]** |
| `0x20` | 1 | `0x00` | **[Confirmed]** |
| `0x21` | 1 | Player number $D$ (1–6) | **[Confirmed]** |
| `0x22`–`0x23` | 2 | Payload length `0x003c` (`0x60 - 0x24`) | **[Confirmed]** |
| `0x24`–`0x27` | 4 | Milliseconds to next beat (at pitched tempo; `0xffffffff` if track ends) | **[Confirmed]** |
| `0x28`–`0x2b` | 4 | Milliseconds to 2nd beat | **[Confirmed]** |
| `0x2c`–`0x2f` | 4 | Milliseconds to next bar (downbeat) | **[Confirmed]** |
| `0x30`–`0x33` | 4 | Milliseconds to 4th beat | **[Confirmed]** |
| `0x34`–`0x37` | 4 | Milliseconds to 2nd bar (downbeat) | **[Confirmed]** |
| `0x38`–`0x3b` | 4 | Milliseconds to 8th beat | **[Confirmed]** |
| `0x54`–`0x57` | 4 | Playback pitch ($0x100000 \times (1 + pct / 100)$) | **[Confirmed]** |
| `0x5a`–`0x5b` | 2 | Track BPM $\times 100$ | **[Confirmed]** |
| `0x5c` | 1 | Beat within bar (1–4) | **[Confirmed]** |

---

## 5. UDP 50002 Media Query (`0x05`) & Media Response (`0x06`)

Used to discover media attributes (name, date, track count, playlist count, capacity, free space) for a given slot.

### 5.1 Media Query (Type `0x05`, 48 bytes / `0x30`)

Sent to port 50002 on the player holding the media slot.

| Offset | Size | Value / Meaning | Status |
| --- | --- | --- | --- |
| `0x00`–`0x09` | 10 | `Qspt1WmJOL` | **[Confirmed]** |
| `0x0a` | 1 | Packet type `0x05` | **[Confirmed]** |
| `0x0b`–`0x1e` | 20 | Device name (NUL-padded) | **[Confirmed]** |
| `0x1f` | 1 | `0x01` | **[Confirmed]** |
| `0x20` | 1 | Subtype `0x00` | **[Confirmed]** |
| `0x21` | 1 | Posing player number $D$ | **[Confirmed]** |
| `0x22`–`0x23` | 2 | Remaining length: `0x000c` (12 bytes) | **[Confirmed]** |
| `0x24`–`0x27` | 4 | Reply IPv4 address (our IP) | **[Confirmed]** |
| `0x28`–`0x2a` | 3 | Padding `00 00 00` | **[Confirmed]** |
| `0x2b` | 1 | Target device number $D_r$ | **[Confirmed]** |
| `0x2c`–`0x2e` | 3 | Padding `00 00 00` | **[Confirmed]** |
| `0x2f` | 1 | Target slot $S_r$ (`0x02` SD, `0x03` USB) | **[Confirmed]** |

### 5.2 Media Response (Type `0x06`, 192 bytes / `0xc0`)

Replied by the player to port 50002 of the query IP address.

| Offset | Size | Value / Meaning | Status |
| --- | --- | --- | --- |
| `0x00`–`0x09` | 10 | `Qspt1WmJOL` | **[Confirmed]** |
| `0x0a` | 1 | Packet type `0x06` | **[Confirmed]** |
| `0x0b`–`0x1e` | 20 | Device name (NUL-padded) | **[Confirmed]** |
| `0x1f` | 1 | `0x01` | **[Confirmed]** |
| `0x20` | 1 | Subtype `0x00` | **[Confirmed]** |
| `0x21` | 1 | Posing player number $D$ | **[Confirmed]** |
| `0x22`–`0x23` | 2 | Remaining length: `0x009c` (156 bytes) | **[Confirmed]** |
| `0x24`–`0x26` | 3 | Padding `00 00 00` | **[Confirmed]** |
| `0x27` | 1 | Target player number $D_r$ | **[Confirmed]** |
| `0x28`–`0x2a` | 3 | Padding `00 00 00` | **[Confirmed]** |
| `0x2b` | 1 | Target slot $S_r$ (`0x02` SD, `0x03` USB) | **[Confirmed]** |
| `0x2c`–`0x6b` | 64 | Media Name (UTF-16BE, NUL-padded) | **[Confirmed]** |
| `0x6c`–`0x87` | 28 | Creation Date string (UTF-16BE, e.g. `2014-06-21`) | **[Confirmed]** |
| `0x88`–`0xa5` | 30 | UTF-16 text (`1000`) | **[Confirmed]** |
| `0xa6`–`0xa7` | 2 | Track count (uint16 big-endian) | **[Confirmed]** |
| `0xa8` | 1 | UI Color tint: `0` default, `1` pink, `2` red, `3` orange, `4` yellow, `5` green, `6` aqua, `7` blue, `8` purple | **[Confirmed]** |
| `0xaa` | 1 | Track type $T_r$: `0x01` rekordbox, `0x02` unanalyzed | **[Confirmed]** |
| `0xab` | 1 | My Settings flag: `1` if `/PIONEER/MYSETTING.DAT` exists | **[Confirmed]** |
| `0xae`–`0xaf` | 2 | Playlist count (uint16 big-endian) | **[Confirmed]** |
| `0xb0`–`0xb7` | 8 | Total storage capacity in bytes (uint64 big-endian) | **[Confirmed]** |
| `0xb8`–`0xbf` | 8 | Free storage capacity in bytes (uint64 big-endian) | **[Confirmed]** |

---

## 6. TCP 12523 dbserver Protocol

Pro DJ Link media browsing uses a binary RPC protocol over TCP.

1. **Port query**: Connect to TCP 12523, write `00 00 00 0f` + ASCII `RemoteDBServer\0`, read 2-byte big-endian port number.
2. **Greeting**: On the resolved port, send `11 00 00 00 01` (5 bytes), expect identical `11 00 00 00 01` reply.
3. **Setup request**:
   - Transaction ID: `0xfffffffe`
   - Request kind: `0x0000`
   - Argument: our player number (e.g. `1`)
   - Wire hex: `11 87 23 49 ae 11 ff ff ff fe 10 00 00 0f 01 14 00 00 00 0c 06 00 00 00 00 00 00 00 00 00 00 00 11 00 00 00 01`
   - Response kind: `0x4000`, argument 1 is the server's player number.
4. **Wire Framing**:
   - Magic: `11 87 23 49 ae` (5 bytes)
   - Transaction: `11` + uint32 (5 bytes)
   - Message kind: `10` + uint16 (3 bytes)
   - Argument count: `0f` + uint8 (2 bytes)
   - Tag descriptor: `14 00 00 00 0c` + 12 tag bytes (each tag: `0x06` number, `0x02` string, `0x03` blob)
   - Encoded arguments:
     - Number: `11` + uint32 (5 bytes)
     - String: `26` + uint32 code units (including NUL terminator) + UTF-16BE bytes
     - Blob: `14` + uint32 length + raw bytes
5. **DMST parameter**:
   Argument 0 of all menu queries:
   `DMST = (player << 24) | (menu_location << 16) | (slot << 8) | track_type`
   For USB slot on player 1: `0x01010301`.
6. **Common Queries & Answers**:
   - `0x1000` Root menu: args `[DMST, 0, 0x00ffffff]`.
   - `0x1004` All tracks: args `[DMST, 0]`.
   - `0x1105` Playlist children: args `[DMST, sort=0, playlist_id, is_folder=1|0]`.
   - `0x2102` Track info / file path: args `[DMST, track_id]`.
   - `0x3000` Render items: args `[DMST, offset, count <= 64, 0, total, 0]`.
   - Answers:
     - `0x4001`: Render header (arg 0: items remaining, arg 1: total).
     - `0x4101`: Render item (12 arguments: parent ID, item ID, label length, label string, secondary length, secondary string, kind, 0, 0, position, 0, 0). Root labels wrapped in `￺ ... ￻`.
     - `0x4201`: Render footer (0 arguments, signals query completion).
     - `0x4003`: Resource unavailable / error.

---

## 7. ANLZ Files & Rekordbox Database Structure

ANLZ files (`.DAT`, `.EXT`, `.2EX`) reside in `/PIONEER/USBANLZ/` and provide beat grids, cues, waveforms, and track paths.
`export.pdb` and `exportExt.pdb` reside in `/PIONEER/rekordbox/` using the DeviceSQL format.

### 7.1 ANLZ File Header (`PMAI`)

| Offset | Size | Field | Meaning |
| --- | --- | --- | --- |
| `0x00`–`0x03` | 4 | Magic | ASCII `PMAI` (`0x50 0x4d 0x41 0x49`) |
| `0x04`–`0x07` | 4 | Header Length | Size of header in bytes (typically 28 bytes) |
| `0x08`–`0x0b` | 4 | File Length | Total file length in bytes |
| `0x0c`–`0x1b` | 16 | Reserved | Header reserved fields |

Following the header is a stream of tagged sections, each starting with:
- 4-byte ASCII tag (`fourcc`)
- 4-byte header length (`len_header`)
- 4-byte total tag length (`len_tag`, including 12 bytes of header)
- Tag body (`len_tag - 12` bytes)

### 7.2 Key ANLZ Tags

- **`PQTZ` (Beat Grid, `.DAT`)**:
  - Header: 24 bytes (4 bytes padding, `0x00080000`, 4-byte `entry_count`).
  - Entries: 8 bytes per beat:
    - Beat within bar: uint16 (1–4)
    - Tempo: uint16 (BPM $\times 100$)
    - Time: uint32 (milliseconds from track start)
- **`PWV5` (Color Waveform Detail, `.EXT`)**:
  - Header: 24 bytes (`entry_bytes = 2`, `len_entries`, `0x00960000` or reserved).
  - Entries: uint16 per column (150 columns/sec):
    - Bits 15–13: Red intensity (3 bits, 0–7)
    - Bits 12–10: Green intensity (3 bits, 0–7)
    - Bits 9–7: Blue intensity (3 bits, 0–7)
    - Bits 6–2: Height (5 bits, 0–31)
    - Bits 1–0: Reserved (`00`)
- **`PWV7` (3-Band Waveform Detail, `.2EX`)**:
  - Header: 24 bytes (`entry_bytes = 3`, `len_entries`, `0x00960000` or reserved).
  - Entries: 3 bytes per column: `[mid, high, low]` amplitude levels (0–31).
- **`PPTH` (Track Audio Path, `.DAT`)**:
  - Header: 16 bytes (`len_path` uint32).
  - Body: UTF-16BE path to audio file on drive (e.g. `/Contents/Artist/Track.mp3`).
- **`PCOB` / `PCO2` (Cues & Loops)**:
  - Memory cues and hot cues (A–H) with timestamp ms and loop endpoints.

### 7.3 `export.pdb` (DeviceSQL Relational Database)

The database consists of fixed-size pages (typically 4096 bytes).
- Page 0 contains the file header, page size, sequence number, and table descriptor list.
- Table page types include:
  - `0`: Tracks (IDs, title, artist ID, album ID, genre ID, BPM, key ID, rating, path)
  - `1`: Genres
  - `2`: Artists
  - `3`: Albums
  - `4`: Labels
  - `5`: Musical Keys
  - `6`: Color Labels
  - `7`: Playlist Tree & Entries
  - `8`: History Playlists & Entries
  - `9`: Artwork
- Row storage: Heap rows indexed backwards from the end of each page, using a bitmask for active rows. Strings are stored as Pascal-style ASCII or UTF-16LE.
