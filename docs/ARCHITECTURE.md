# OpenCDJ-3000 architecture

## Goal

A virtual player that a Pro DJ Link implementation can be tested against without hardware: it plays the CDJ side, the device under test plays its own side, and the browser shows both sides plus the wire.

Today the device under test is EngineX OS (Denon / Numark decks): CDJ → Denon (#51 live waveforms, #59 USB import) and Denon → CDJ (#63 export, on hold upstream until a real CDJ is available).

## Components

```
 browser ──HTTP──▶ 127.0.0.1:18300  cdj3000-sim (one process, loopback only)
                   │
                   ├─ Virtual CDJ-3000            sim-ip 127.0.0.2
                   │    keepalive 50000 / beat 50001 / status 50002  ──▶ device under test
                   │    NFSv2 server  /C/ = USB root (portmap, mount, nfs)  ◀── device fetches ANLZ, audio, export.pdb
                   │    dbserver client (TCP 12523) + NFS client  ──▶ device's export (#63)
                   │    listeners 127.0.0.2:50000/50002  ◀── device's keepalive / status
                   │
                   ├─ Device under test            third_party/enginex (EngineX OS, read-only copy)
                   │    prolink.Discovery on denon-ip:50000-50002 (receiver, #51)
                   │    prolink.Stage + bridge.ConvertAtMount (USB import, #59)
                   │    prolink.Exporter on 127.0.0.1, random ports (export, #63)
                   │
                   └─ HTTP API + web UI (embedded index.html), wire log ring buffer (400 entries)
```

| Piece | File | Notes |
| --- | --- | --- |
| CDJ wire protocol, dbserver client, ANLZ | `pkg/cdj/` | Independent builders/parsers (`0x06` keepalive, `0x0a` status, `0x28` beat, `0x0b` 30 ms playhead, `0x05/0x06` media query/response), TCP 12523 client, ANLZ (`PQTZ`/`PWV5`/`PWV7`), captured-wire tests |
| Play head, beat clock, HTTP control | `cmd/cdj3000-sim/main.go` | Status every 200 ms, keepalive every 1.5 s and at once on a number change, `0x0b` playhead every 30 ms, beat packet on each beat while playing |
| Generated USB stick | `cmd/cdj3000-sim/fixture.go` | 64 s of drums at 120 BPM; audio, PQTZ, PWV5 and PWV7 come from the same synthesized bands |
| Reverse link, media query reply, collision watch | `cmd/cdj3000-sim/link.go` | Uses `pkg/cdj` dbserver client and replies `0x06` to incoming `0x05` media queries |
| Web UI | `cmd/cdj3000-sim/index.html` | No build step, no external assets |
| Device under test | `third_party/enginex/` | Synced by `scripts/sync-enginex.sh`; `VERSION` records the source commit |

## HTTP API

| Method and path | Purpose |
| --- | --- |
| `GET /api/state` | Virtual CDJ state, device-under-test snapshot (waveforms stripped), export status, yields, last 120 wire entries |
| `POST /api/control` | `{"number","playing","pitch","pitch_max","track","cue"}`; pitch is clamped to the range |
| `GET /api/waveform?id=&side=` | Full waveform and beatgrid of a USB track, or what the device fetched (`side=denon`) |
| `GET /audio?id=` | Audio of a USB track, only paths listed in export.pdb inside the USB root |
| `POST /api/stage` | CDJ → Denon import, returns staged file count and the Engine playlists |
| `POST /api/export?enabled=0|1` | Turn the device's export on or off |
| `GET /api/link/menu?q=root|tracks|folder|playlist&id=` | dbserver browse of the device's export |
| `POST /api/link/load?id=` · `GET /api/link/audio` | Load a track from the device over dbserver + NFSv2, then play it |

## Test strategy

1. **Independent sides.** The simulator's packet builders and dbserver client are written from Deep Symmetry's analysis, not shared with the device's code. A shared encoder would make both sides agree on the same mistake.
2. **Loopback only.** Everything binds to 127.0.0.x. Nothing is announced on a real LAN, where a stray player number could confuse players in a club. A future LAN mode must be opt-in and passive by default.
3. **Evidence over agreement.** A green run proves only that both sides agree. Real-hardware captures (Deep Symmetry's pcaps, and our own once we have a player) are the reference. A test that contradicts a capture is wrong.
4. **Tests** live next to the code (`cmd/cdj3000-sim/main_test.go`): the device sees a playing CDJ-3000 with NFS waveform and beatgrid, the #59 import builds the `[Pro DJ Link: CDJ-3000]` folder, #63 browse and load work, and the export yields 4 → 3. They use their own loopback addresses so they can run beside the device's own tests.

## Roadmap

| Milestone | Scope |
| --- | --- |
| M1 | Move CDJ-side code from `cmd/` into `pkg/cdj` (packets, clock, dbserver client, NFS server) with byte-level tests against Deep Symmetry's documented examples |
| M2 | Replay real captures from `/root/build/sc5000-lab/deep-symmetry-archive/` and diff the device's decoded state against beat-link's |
| M3 | Several virtual players at once (1–4, plus a DJM mixer's on-air and fader-start packets) |
| M4 | CDJ-2000NXS2 profile (`0x11c`/`0x124` status) next to CDJ-3000 (`0x200`) |
| M5 | Opt-in LAN mode on a dedicated interface or VLAN, to test real decks, passive listen first |
| M6 | Record and export sessions as pcap for bug reports |
