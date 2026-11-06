# OpenProLink release checklist

Releases are made only when the maintainer asks for one. Versions are `v0.<minor>.<patch>` until the wire protocol is checked against real hardware.

1. **Issue and commits**: every change in the release has an Issue and is pushed to `origin/main`.
2. **Tests on the build host** (alias in `CLAUDE.local.md`) from a clean directory:
   `REMOTE_BUILD_HOST=<alias> remote-build --clean --name openprolink-release -- '. /etc/profile.d/buildtools.sh; gofmt -l cmd pkg; go vet ./... && go test -race -count=1 ./...'`
3. **Binaries**: `cdj3000-sim` for `linux/arm64` and `linux/amd64`; `prolink-probe` also for `darwin/arm64`, `darwin/amd64` and `windows/amd64`. `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`, named `<tool>-<version>-<os>-<arch>[.exe]`, plus `SHA256SUMS.txt`.
4. **Smoke run**: start the arm64 binary on the build host, open the UI through the tunnel, check that the device-under-test panel shows a playing CDJ-3000 with waveform, the #59 import lists `[Pro DJ Link: CDJ-3000]`, and the LINK panel browses and loads a track.
5. **Release**: `gh release create v<version> --repo Cd1s/OpenProLink --title "OpenProLink <version>" --notes-file notes.md <binaries> SHA256SUMS.txt`. The notes list the Issues, the `third_party/enginex/VERSION` line and the SHA-256 table.
6. **Verify**: `gh release view v<version> --repo Cd1s/OpenProLink --json assets` digests match `SHA256SUMS.txt`.
7. **Never** publish a release that claims compatibility with a real player unless it was tested on one; say "tested against EngineX OS <commit> in the simulator" otherwise.
