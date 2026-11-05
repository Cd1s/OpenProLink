# Contributing to OpenCDJ-3000

OpenCDJ-3000 exists so DJ software and firmware can be tested without a booth full of players. The bar: never confuse a real network, and never claim compatibility that only our own two sides agree on.

## Issues

- One Issue per bug or feature, opened before the work starts.
- For protocol bugs, attach a capture (pcap) or point at the Deep Symmetry analysis section.
- Remove keys, tokens and private host names from logs.

## Pull requests

1. Branch from `main`, one topic per PR, commit messages `type(scope): summary`.
2. New packet fields or dbserver requests come with a byte-level test.
3. Do not edit `third_party/enginex`; run `scripts/sync-enginex.sh` and fix bugs upstream.

### Ground rules

- **Loopback by default.** No packet leaves 127.0.0.0/8 unless a future, explicit LAN mode is turned on.
- **Independent sides.** The CDJ side never imports the device's encoders.
- **Mark guesses.** A constant whose meaning is unknown is marked *unverified* in `docs/PROLINK-WIRE-SPEC.md`.

### Tests

On a Linux host with Go (the maintainers use `<build-host>`):

```sh
gofmt -l cmd && go vet ./... && go test -race ./...
```
