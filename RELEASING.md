# Releasing

Releases are built by GoReleaser in GitHub Actions
(`.github/workflows/release.yml`) when a tag starting with `v` is pushed.

```
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Tags are signed (`-s`). A tag with a suffix, such as `v0.0.1-beta.1` or
`v0.2.0-rc.1`, is published as a pre-release.

The workflow runs the tests, builds Linux (amd64, arm64, armv7) and macOS
(amd64, arm64) binaries, and publishes them with checksums, `.rpm` and `.deb`
packages and a changelog on the GitHub release. There is no Windows build: oob
needs SIGWINCH, flock and a pty. The version is stamped into the binary, so
`oob version` shows it. It runs on `ubuntu-latest` because there is no cgo, so
every target cross-compiles.

Actions are pinned by full commit SHA and GoReleaser by exact version. Bump them
together, taking the SHA from the release tag (`git ls-remote`).

Each release also updates the Homebrew casks in
[ssimpson89/homebrew-tap](https://github.com/ssimpson89/homebrew-tap).
`brew install ssimpson89/tap/oob@beta` gets every release.
`brew install ssimpson89/tap/oob` gets stable releases only, except that
until the first stable tag exists it gets the betas too. That needs a
repository secret, `HOMEBREW_TAP_TOKEN`: a fine-grained personal access token
with **Contents: read and write** on `homebrew-tap` only. Without it the
Homebrew step is skipped and the rest of the release goes ahead. The cask
takes the unsigned macOS binary out of quarantine after installing, so
Gatekeeper doesn't block it.

To try a release locally without publishing:

```
make snapshot        # goreleaser release --snapshot --clean
```

## RPM signing (optional)

RPMs are signed when the repository secrets `RPM_SIGNING_KEY` (an armored
private key) and `RPM_SIGNING_PASSPHRASE` exist, and built unsigned otherwise.
`.deb` files are not signed; apt checks a signed repository index instead.

## macOS signing (optional, paid)

Unsigned macOS binaries work, but a binary downloaded with a browser is
quarantined and Gatekeeper refuses to open it until the user allows it in
System Settings, Privacy & Security, or runs
`xattr -d com.apple.quarantine oob`. `go install`, `curl` and Homebrew
casks don't leave the quarantine flag.

Signing and notarizing need a paid Apple Developer Program membership. GoReleaser
does both from CI once these repository secrets exist; until then the
`notarize` step is skipped.

| Secret | What it is |
|---|---|
| `MACOS_SIGN_P12` | Developer ID Application certificate and key, exported as .p12, base64-encoded |
| `MACOS_SIGN_PASSWORD` | Password for the .p12 |
| `MACOS_NOTARY_ISSUER_ID` | Issuer ID from App Store Connect, Users and Access, Integrations, Keys |
| `MACOS_NOTARY_KEY_ID` | ID of an App Store Connect API key |
| `MACOS_NOTARY_KEY` | That key's `AuthKey_XXXX.p8` file, base64-encoded |
