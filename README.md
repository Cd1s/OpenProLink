<p align="center"><img src="./docs/assets/hero-banner.svg" alt="OpenProLink" width="100%"></p>

<p align="center"><b>English</b> · <a href="./README.zh-CN.md">简体中文</a> · <a href="./README.zh-TW.md">繁體中文</a></p>

# OpenProLink

**Open-source Pioneer DJ / AlphaTheta Pro DJ Link hardware simulator (CDJ-3000 / XDJ) and full-protocol capture suite (CDJ / XDJ / DJM mixers).**

OpenProLink has two tools:

- **`prolink-probe`** records what real Pioneer players and mixers send: every packet kind with its send rate, raw hex samples, a decoded view and a map of the bytes that changed while you worked the controls. One run on your gear gives developers what they otherwise need the hardware on their desk for.
- **`cdj3000-sim`** is a software player for Pro DJ Link test benches. It joins the link as a virtual CDJ-3000 and serves a rekordbox USB stick to the device under test. You watch and control it from a web page: what the deck sends, what the other side understands, and every packet in between.

It was built for [EngineX OS](https://github.com/Cd1s/EngineX-OS), the custom system for Denon DJ / Numark / Rane standalone units, which talks Pro DJ Link to Pioneer players.

> **Status: early.** The simulator's packet layouts follow Deep Symmetry's protocol analysis (`dysentery`, `beat-link`, `crate-digger`). A passing run proves that the device under test and this simulator agree; only real hardware proves compatibility. That is why captures from real players and mixers matter: please send one.

## Capture your hardware with prolink-probe

Wanted: **CDJ-3000, CDJ-2000NXS2, XDJ-AZ, XDJ-XZ, DJM-A9, DJM-V10, DJM-900NXS2**, and any other Pro DJ Link player or mixer.

1. Connect your computer to the same switch or router as the devices (or to the LAN port of an XDJ-XZ / XDJ-AZ). Quit rekordbox and any other Pro DJ Link software, which hold the same ports.
2. Install [Go](https://go.dev/dl/) 1.26 or newer, then run from a checkout of this repository:

   ```sh
   go run ./cmd/prolink-probe -duration 60s -out capture.json
   ```

   Allow incoming connections if macOS or Windows asks. Ctrl-C ends the capture early and still writes the report.
3. During the 60 seconds, do **one action at a time, about 3 seconds apart**, and write down roughly when you did each one (see the table below).
4. Review `capture.json`, then attach it to a [hardware capture issue](https://github.com/Cd1s/OpenProLink/issues/new?template=device-capture.md) with your models, firmware versions, how they are connected and your list of actions.

| Device | Actions that reveal the most |
| --- | --- |
| **CDJ-3000 / CDJ-2000NXS2** | Load a track from USB, Play / Pause, move the tempo fader, change the tempo range, Master Tempo on/off, **Quantize on/off**, **MASTER** on this deck then on another, **SYNC** on/off, a loop, a hot cue, eject |
| **XDJ-AZ / XDJ-XZ** | The player actions above on each deck, plus the mixer section: each channel fader up and down, crossfader, **Beat FX** on/off, change the effect, turn the beat / **TIME** knob and the level/depth knob, Quantize |
| **DJM-A9 / DJM-V10 / DJM-900NXS2** | Each **channel fader** up and down one at a time (the players' on-air ring follows), crossfader, **Beat FX** on/off, change the effect and its channel, beat buttons (1/2, 1, 2 …), **TIME** knob, level/depth, the mixer's **Quantize**, tap or auto BPM |

### What the probe does

By default it **listens for 1.5 s**, then joins the link as a virtual player on the **first free number from 7 to 15** (players use 1–6, rekordbox 0x11 and up, mixers 0x21 and up) and announces itself every second. Players and mixers unicast most of their traffic only to devices they have seen announce, so without this step a computer hears little more than keepalives and beats. It then:

- sends each player a **media query (`0x05`)** for USB and SD and records the **media response (`0x06`)**;
- opens each player's **dbserver** (TCP 12523 port query, greeting, setup, root menu and render) and keeps every message as raw hex plus decoded arguments. Players refuse dbserver clients numbered above 4, so it asks as the target player's own number, as beat-link does;
- records **everything received on UDP 50000, 50001 and 50002**: keepalives, CDJ status `0x0a`, mixer status `0x29`, beats `0x28`, the CDJ-3000's 30 ms precise position `0x0b`, channels on air `0x03`, master handoff `0x26`/`0x27`, sync control `0x2a`, and any kind nobody has documented yet.

It never sends status, beat, master, sync, fader or load commands. If a device takes its number it moves to another one. `-passive` only listens and sends nothing.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-duration` | `15s` | How long to capture; use 60 s or more while you work the controls |
| `-out` | `capture.json` | Report file |
| `-passive` | off | Listen only: no announcement, no queries (players then send far less) |
| `-number` | first free 7–15 | Virtual player number to announce |
| `-iface` | the one on the devices' subnet | Network interface to announce on |
| `-samples` | `16` | Raw hex samples kept per packet kind, at most one per second |

### What is in the report

| Key | Contents |
| --- | --- |
| `devices` | Every device heard: name, player number, device class, firmware (from its status), when it was first seen |
| `streams` | One entry per (device, port, type, subtype, length): packet count, interval in ms (avg/min/max), the last packet decoded, raw hex samples |
| `streams[].changed_bytes` | Every offset that changed during the capture, with its known meaning or none (an undocumented field), first/last/min/max value, number of changes, distinct values, and the times of the first and last change. Match those times to your list of actions to find the byte behind a knob |
| `media_queries` | Each `0x05` sent and whether the player answered |
| `dbserver` | Per player and slot: dbserver port, root menu items and the full message transcript |
| `notes` | The player number used, number changes, problems such as no traffic heard |

**Privacy.** IP and MAC addresses inside packets are replaced with stand-ins (`10.0.0.x`, `02:00:00:00:00:xx`). Every other byte is kept exactly as received, including the USB stick's name and the track IDs. A four-byte value that matches a real IP address outside the places the protocol keeps addresses is listed under `unredacted_ip_like` and left untouched; check those before posting.

## Simulator

| Panel | What you get |
| --- | --- |
| **Virtual CDJ-3000** | Player number 1–4, Play / Pause / Cue, tempo ±6 / ±10 / ±16 % / WIDE, beat counter, scrolling 3-band waveform and overview, USB track list, audio in the browser |
| **Device under test** | The [EngineX OS](https://github.com/Cd1s/EngineX-OS) Pro DJ Link receiver running in-process: the players, BPM, pitch, beat and play state it decodes, and the waveform it fetched over NFS |
| **CDJ → Denon import** | Copies the virtual USB over NFSv2 and runs EngineX's rb-bridge, then lists the Engine playlists it built |
| **[LINK] Denon → CDJ** | Browses the EngineX export with a dbserver client (TCP 12523), loads a track over NFSv2, shows its waveform and plays it; shows the export yielding its player number on a collision |
| **Wire inspector** | UDP 50000 keepalive, 50001 beat + 30 ms `0x0b` playhead, 50002 status + `0x05/0x06` media query, dbserver requests and NFS reads, with the first bytes of each in hex |

The default USB stick is generated: 32 bars of 120 BPM drums with a matching beatgrid, PWV5 colour and PWV7 3-band waveforms. Pass `-usb` to use a real rekordbox export.

### Quick start

Build and run on a Linux host (it uses the loopback addresses 127.0.0.1 and 127.0.0.2, and UDP ports 50000–50002):

```sh
go build -trimpath -o bin/cdj3000-sim ./cmd/cdj3000-sim
./bin/cdj3000-sim -http 127.0.0.1:18300                # generated USB stick
./bin/cdj3000-sim -usb /path/to/USB -http 127.0.0.1:18300
```

The web page listens on loopback only. From your laptop:

```sh
ssh -L 18300:127.0.0.1:18300 <build host>
open http://127.0.0.1:18300
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-http` | `127.0.0.1:18300` | Web UI address. Keep it on loopback |
| `-usb` | generated | rekordbox USB root with `PIONEER/rekordbox/export.pdb` |
| `-sim-ip` | `127.0.0.2` | Address the virtual CDJ sends from and serves NFS on |
| `-denon-ip` | `127.0.0.1` | Address the device-under-test receiver listens on |
| `-denon-empty` | off | The Denon drive holds no rekordbox export of its own |
| `-deck` | none | IPv4 of real EngineX decks on the LAN to play to, separated by commas (set `-sim-ip` to this host's LAN address) |
| `-ableton-link` | off | Publish tempo and bar phase as an Ableton Link session |

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/prolink-probe/` | The capture tool and its tests against simulated CDJ-3000 and DJM-V10 hardware |
| `cmd/cdj3000-sim/` | The simulator: playhead clock, generated USB stick, reverse link and web UI |
| `pkg/cdj/` | Independent CDJ-side packet builders/parsers (`0x06`, `0x0a`, `0x28`, `0x0b`, `0x05/0x06`), dbserver client, ANLZ parser and captured-wire tests |
| `third_party/enginex/` | Read-only copy of the EngineX OS packages under test. Refresh with `scripts/sync-enginex.sh`, never edit |
| `docs/ARCHITECTURE.md` | Components, data flow, test strategy and roadmap |
| `docs/PROLINK-WIRE-SPEC.md` | Every packet the simulator sends or parses, byte by byte |

## Trademarks

Pioneer DJ, CDJ, XDJ, DJM, rekordbox and Pro DJ Link are trademarks of AlphaTheta Corporation. Denon DJ and Engine DJ are trademarks of inMusic Brands. This project is not affiliated with or endorsed by either company. Model names describe the hardware it emulates or records.

## License

MIT. `third_party/enginex` is EngineX OS code under its own MIT license.
