# OpenProLink — notes for AI agents

Pro DJ Link simulator (virtual CDJ-3000) and hardware capture suite (`prolink-probe`). Host names, the running instance and archive locations are in `CLAUDE.local.md` (not in git). Read [`CONTRIBUTING.md`](./CONTRIBUTING.md) and [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) first.

- **Roles**: the PM agent plans, dispatches and accepts work; it does not write the main code. Reverse engineering goes to GPT-6.1 Sol (fast), development to Claude Opus 5.5, review to Gemini 3.8 Flash (medium); exact provider ids are in the PM memory, not in the repo. Automated or scheduled runs never use Opus.
- **Issues first**: every feature and fix starts as an Issue in `Cd1s/OpenProLink`. Commits reference it.
- **Build and test only on the maintainers' aarch64 build host** (alias in `CLAUDE.local.md`): `REMOTE_BUILD_HOST=<alias> remote-build -- '. /etc/profile.d/buildtools.sh; go vet ./... && go test -race ./...'`. Never build, test or install dependencies on the maintainer's laptop.
- **Running instance**: see `CLAUDE.local.md`. Keep the previous binary as `.prev` when replacing it.
- **`third_party/enginex` is read-only**: it is the device under test, copied from EngineX OS. Refresh it with `scripts/sync-enginex.sh <EngineX checkout>`, and fix its bugs upstream in `Cd1s/EngineX-OS`.
- **Independent sides**: the CDJ side (packets, dbserver client, NFS reads) must not reuse the device's encoders, or both sides agree on the same mistake. Real captures beat our own assumptions.
- **Loopback only** for the simulator: never announce on a real LAN by default; a stray player number can confuse real players in a club. `prolink-probe` is the one exception, by design: player 7–15 only, keepalive + media query + dbserver reads only, yields its number on a conflict.
- **References**: Deep Symmetry `dysentery`, `crate-digger`, `beat-link`; `pyrekordbox`; `libdjinterop`. Archived on the build host (path in `CLAUDE.local.md`).
- **Ponytail full**: the smallest complete change; end every report with `Skipped:` and `Risk:`.
- **Commits**: `type(scope): summary`, no AI co-author trailers, no keys, tokens, passwords, private domains or private host names (`git grep` them before every push: the repository is public).
- **Name**: OpenProLink (renamed from OpenCDJ-3000 on 2026-11-06, chosen by the maintainer). "CDJ", "XDJ", "DJM" and "Pro DJ Link" are Pioneer DJ / AlphaTheta trademarks: keep the trademark notice in all three READMEs.
- **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`) change together and keep the same layout.
