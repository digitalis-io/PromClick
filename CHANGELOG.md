# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- In-memory query-result cache for `promclick-proxy`. When `cache.enabled: true`,
  `/api/v1/query` and `/api/v1/query_range` responses are cached in an LRU keyed
  by `(query, start, end, step)` with per-entry TTL, and concurrent identical
  queries are collapsed into a single evaluation via singleflight. Windows whose
  end is within `cache.max_freshness` of now are served but not stored (their
  data is still being written). Config: `cache.{enabled,max_size,ttl,max_freshness}`
  (previously present but unwired; now active, default disabled). New package
  `proxy/cache`.
- `match[]` (plus best-effort `start`/`end`) support on `/api/v1/labels` and
  `/api/v1/label/{name}/values`, so Grafana template variables such as
  `label_values(up, instance)` are scoped to the selected metric instead of
  returning every value across the whole TSDB. `POST` is now accepted on
  `/api/v1/label/{name}/values` alongside `GET`, matching Prometheus.
- `/api/v1/metadata` is now populated (one `unknown`-typed entry per metric name)
  and honours the `metric` and `limit` parameters, so Grafana's metric browser
  and autocomplete work. Previously returned an empty object.
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
- `/api/v1/series` now parses each `match[]` with the Prometheus parser, applies
  **all** label matchers (not just a single metric name), supports multiple
  `match[]` selectors, deduplicates by series fingerprint, and answers from the
  in-memory label cache when a concrete metric name is present. Its ClickHouse
  fallback now decodes the JSON-string `labels` column correctly (the previous
  hand-rolled parser silently returned empty label sets for `String` label
  columns).
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
- Reused the existing `chEscape` for all matcher values rendered into ClickHouse
  metadata/series SQL (equality, inequality and regex conditions), keeping the
  `match[]`-driven query paths free of SQL injection.
- Fixed an SQL-injection vector in the OTel metadata/series handlers: `chEscape`
  now escapes backslashes before single quotes so ClickHouse string literals cannot
  be broken out of via a trailing backslash in request parameters.

## [0.1.0]

Initial upstream release vendored into the Digitalis.io distribution. See the
[upstream project](https://github.com/PromClick/PromClick) for the original history.
