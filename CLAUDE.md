# OpenCDJ-3000 — notes for AI agents

Virtual CDJ-3000 and Pro DJ Link testbench. Read [`CONTRIBUTING.md`](./CONTRIBUTING.md) and [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) first.

- **Roles**: the PM agent plans, dispatches and accepts work; it does not write the main code. Reverse engineering goes to GPT-6.1 Sol (fast), development to Claude Opus 5.5, review to Gemini 3.8 Flash (medium); exact provider ids are in the PM memory, not in the repo. Automated or scheduled runs never use Opus.
- **Issues first**: every feature and fix starts as an Issue in `Cd1s/OpenCDJ-3000` (private for now). Commits reference it.
- **Build and test only on `<build-host>`** (ssm alias, aarch64): `REMOTE_BUILD_HOST=<build-host> remote-build -- '. /etc/profile.d/buildtools.sh; go vet ./... && go test -race ./...'`. Never build, test or install dependencies on the maintainer's laptop.
- **Running instance**: transient systemd unit `cdj3000-sim` on `<build-host>`, binary `/root/opencdj3000/cdj3000-sim`, UI on `127.0.0.1:18300` there, reached through `sshctl tunnel <build-host> -L 127.0.0.1:18300:127.0.0.1:18300`. Keep the previous binary as `.prev` when replacing it.
- **`third_party/enginex` is read-only**: it is the device under test, copied from EngineX OS. Refresh it with `scripts/sync-enginex.sh <EngineX checkout>`, and fix its bugs upstream in `Cd1s/EngineX-OS`.
- **Independent sides**: the CDJ side (packets, dbserver client, NFS reads) must not reuse the device's encoders, or both sides agree on the same mistake. Real captures beat our own assumptions.
- **Loopback only**: never announce on a real LAN by default. A stray player number can confuse real players in a club.
- **References**: Deep Symmetry `dysentery`, `crate-digger`, `beat-link`; `pyrekordbox`; `libdjinterop`. Archived on `<build-host>` in `/root/build/sc5000-lab/deep-symmetry-archive/`.
- **Ponytail full**: the smallest complete change; end every report with `Skipped:` and `Risk:`.
- **Commits**: `type(scope): summary`, no AI co-author trailers, no keys, tokens, passwords or private domains.
- **Before going public**: confirm the project name with the maintainer, because "CDJ" is a Pioneer DJ / AlphaTheta trademark. Keep the trademark notice in all three READMEs.
- **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`) change together and keep the same layout.
