---
name: Hardware capture (CDJ / XDJ / DJM)
about: Share a prolink-probe report from real Pioneer DJ / AlphaTheta players or mixers
title: "Hardware capture: <models>"
labels: hardware
---

**Devices** (model and firmware of each, e.g. `CDJ-3000 3.20, DJM-V10 1.30`):

**Connection** (switch / router / XDJ LINK port, which player numbers are set):

**Command and OS** (e.g. `go run ./cmd/prolink-probe -duration 60s`, macOS 15):

**What you did, with the second it happened** (one action at a time, about 3 s apart):

| Second | Device | Action |
| --- | --- | --- |
| 5 | DJM-V10 | Beat FX on |
| 9 | DJM-V10 | TIME knob from 1/2 to 2 |
| 13 | CDJ-3000 #2 | Quantize off |

**Report:** attach `capture.json`. IP and MAC addresses are already replaced; everything else is the raw packets. Check `unredacted_ip_like` entries, and leave the report out if your USB stick's name or anything else in it is private.

**Anything else you noticed:**
