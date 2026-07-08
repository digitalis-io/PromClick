# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `NOTICE` file crediting the upstream [PromClick Authors](https://github.com/PromClick/PromClick)
  and recording the Digitalis.io distribution copyright, preserving Apache-2.0 attribution.
- GitHub Actions workflow (`.github/workflows/build.yml`) that builds the multi-arch
  container image (`linux/amd64`, `linux/arm64`) and publishes it to
  `ghcr.io/digitalis-io/promclick` on pushes to `main` and on tags (`v*`), and can be
  triggered manually. Pull requests build the image without pushing. Actions are pinned
  to commit SHAs; the job has a build timeout and cancels superseded runs.
- Digitalis.io branding and an attribution section in the README.
- `.dockerignore` to shrink the build context.

### Changed
- Container image references now point to `ghcr.io/digitalis-io/promclick` instead of
  the upstream `quay.io/hinski/promclick` (Helm chart values, `docker-compose.yaml`,
  README examples).
- Helm chart `home`, `sources`, and `maintainers` metadata set to the Digitalis.io
  distribution; chart version bumped to `1.1.0`.
- README clone URLs and Helm `oci://` pull path updated to the `digitalis-io` org.
- `Dockerfile` now cross-compiles the Go binaries on the native build platform
  (`GOARCH=$TARGETARCH`, CGO disabled) instead of building under QEMU emulation,
  making multi-arch image builds substantially faster.

### Security
- Fixed an SQL-injection vector in the OTel metadata/series handlers: `chEscape`
  now escapes backslashes before single quotes so ClickHouse string literals cannot
  be broken out of via a trailing backslash in request parameters.

## [0.1.0]

Initial upstream release vendored into the Digitalis.io distribution. See the
[upstream project](https://github.com/PromClick/PromClick) for the original history.
