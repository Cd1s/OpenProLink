# Contributing to OpenProLink

OpenProLink exists so DJ software and firmware can be tested without a booth full of players, and so the people who own the hardware can record what it really sends. The bar: never confuse a real network, and never claim compatibility that only our own two sides agree on.

## Issues

- One Issue per bug or feature, opened before the work starts.
- For protocol bugs, attach a `prolink-probe` report or a pcap, or point at the Deep Symmetry analysis section.
- Remove keys, tokens and private host names from logs.

## Pull requests

1. Branch from `main`, one topic per PR, commit messages `type(scope): summary`.
2. New packet fields or dbserver requests come with a byte-level test.
3. Do not edit `third_party/enginex`; run `scripts/sync-enginex.sh` and fix bugs upstream.

### Ground rules

- **Loopback by default.** The simulator sends nothing outside 127.0.0.0/8 unless `-deck` names a real deck. `prolink-probe` announces on the LAN by design, but only as player 7–15, only keepalives, media queries and dbserver reads, and it moves off a number another device claims.
- **Independent sides.** The CDJ side never imports the device's encoders.
- **Mark guesses.** A constant whose meaning is unknown is marked *unverified* in `docs/PROLINK-WIRE-SPEC.md`.

### Tests

On a Linux host with Go (the simulator tests need the loopback addresses 127.0.0.2 and up, which Linux has and macOS does not):

```sh
gofmt -l cmd pkg && go vet ./... && go test -race ./...
```
