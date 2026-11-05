# Pro DJ Link wire spec (as implemented)

What `cmd/cdj3000-sim` sends and parses. Offsets are hexadecimal, integers big-endian. The reference is Deep Symmetry's analysis (<https://djl-analysis.deepsymmetry.org/>). Fields marked *unverified* are constants we copy without knowing their meaning; check them against a real capture before relying on them.

## Common header

| Offset | Size | Value |
| --- | --- | --- |
| 00 | 10 | `Qspt1WmJOL` |
| 0a | 1 | Packet type |
| 0b or 0c | 20 | Device name, NUL padded (`CDJ-3000`). Offset 0b on ports 50001/50002, 0c on 50000 |
| name + 20 | 1 | `01` |

## UDP 50000 keepalive, type `06`, 54 bytes (0x36)

| Offset | Size | Value |
| --- | --- | --- |
| 21 | 1 | `02` |
| 22 | 2 | Packet length (0x36) |
| 24 | 1 | Player number 1–4 |
| 25 | 1 | `01` |
| 26 | 6 | MAC. The simulator uses a fixed `02:00:00:00:00:20`: a real player keeps its MAC when its number changes, and a changing MAC looks like a second player to the collision logic |
| 2c | 4 | IPv4 address of the player |
| 30 | 1 | `02` (peers seen, *unverified*) |
| 34 | 1 | `01` (*unverified*) |

Sent every 1.5 s, and at once when the player number changes.

## UDP 50002 CDJ status, type `0a`, 512 bytes (0x200, CDJ-3000)

CDJ-2000NXS2 sends 0x11c or 0x124 bytes. The length field at 22 is `size − 0x24`.

| Offset | Size | Value |
| --- | --- | --- |
| 20 | 1 | `06` (*unverified*) |
| 21 | 1 | Player number |
| 22 | 2 | `size − 0x24` |
| 24 | 1 | Player number |
| 26, 27 | 1 each | `01`, `01` (activity, *unverified*) |
| 28 | 1 | Track source player (itself) |
| 29 | 1 | Track source slot: `03` USB (`02` SD) |
| 2a | 1 | Track type: `01` rekordbox |
| 2c | 4 | rekordbox track ID |
| 6a, 6b, 73, 75 | 1 each | `04`, `04`, `04`, `01` (*unverified*) |
| 7b | 1 | P1 play state: `03` playing, `05` paused |
| 7c | 4 | Firmware string `3.20` |
| 89 | 1 | Flags: `40` play, `20` master, `10` sync, `08` on air (`f8` playing, `b8` paused) |
| 8b | 1 | P2: `fa` playing, `fe` stopped |
| 8c, 98, c0, c4 | 4 each | Pitch: `0x100000 × (1 + pct/100)`; `0x108000` = +3.125 % |
| 90 | 2 | `8000` |
| 92 | 2 | Track BPM × 100 |
| 9f | 1 | `ff` |
| a0 | 4 | Beat number, 1-based |
| a6 | 1 | Beat within bar 1–4 |
| b7 | 1 | `01` media present |

Sent every 200 ms.

## UDP 50001 beat, type `28`, 96 bytes (0x60)

| Offset | Size | Value |
| --- | --- | --- |
| 21 | 1 | Player number |
| 24, 28, 2c, 30, 34, 38 | 4 each | ms to next beat, 2nd beat, next bar, 4th beat, 2nd bar, 8th beat, at the pitched tempo |
| 54 | 4 | Pitch, as in status |
| 5a | 2 | Track BPM × 100 |
| 5c | 1 | Beat within bar |

Sent on each new beat while playing.

## TCP 12523 dbserver

1. **Port query**: connect to 12523, send `00 00 00 0f` + `RemoteDBServer\0`, read a 2-byte port. EngineX serves dbserver on the same port.
2. **Greeting**: send number field `11 00 00 00 01`, expect the same back.
3. **Setup**: transaction `fffffffe`, type `0000`, one argument = our player number. The answer has type `4000` and argument 1 = the server's player number.
4. **Messages**: magic `11 87 23 49 ae`, transaction (`11` + u32), type (`10` + u16), argument count (`0f` + u8), argument tag blob (`14` + u32 length 12 + 12 tag bytes: `06` number, `02` string, `03` blob), then the arguments. Numbers are `11` + u32. Strings are `26` + u32 code units (with the NUL) + UTF-16BE.
5. **DMST** (argument 0 of every request) = `player << 24 | menu location << 16 | slot (03 USB) << 8 | track type (01)`.

| Request | Arguments after DMST | Answer |
| --- | --- | --- |
| `1000` root menu | `0, ffffff` | items PLAYLIST (id 5, kind `84`), TRACK (id 4, kind `83`) |
| `1004` all tracks | `0` | track items (kind `0704`) sorted by title |
| `1105` playlist | `sort 0, id, folder 1|0` | folder: children (kind `1` folder, `8` playlist); playlist: its tracks |
| `2102` track info | `track id` | one item, label = audio path on the stick |
| `3000` render | `offset, count ≤ 64, 0, total, 0` | `4001` header, `count` × `4101` item, `4201` footer |

A menu item has 12 arguments: parent, id, label length, label, label2 length, label2, kind, 0, 0, position, 0, 0. Labels of root items are wrapped in `U+FFFA … U+FFFB`. `4003` means unavailable.

## NFSv2 over UDP

The portmapper maps the mount and NFS programs. The export of the USB slot is `/C/` (`/B/` for SD). Path components are UTF-16LE. Files are read 8 KiB at a time. The simulator serves the USB root read-only and reads the device's export the same way: `export.pdb`, then the track's `.DAT` / `.EXT` / `.2EX`, then the audio.

## ANLZ written by the generated stick

| File | Section | Payload |
| --- | --- | --- |
| `.DAT` | `PQTZ` (header 24) | `0`, `0x80000`, count, then 8 bytes per beat: beat in bar (u16), BPM × 100 (u16), time ms (u32) |
| `.EXT` | `PWV5` (header 24) | entry bytes 2, count, 0, then u16 per column: RGB 3 bits each at bits 13, 10, 7, height 5 bits at bit 2 |
| `.2EX` | `PWV7` (header 24) | entry bytes 3, count, 0, then `[mid, high, low]` per column |
