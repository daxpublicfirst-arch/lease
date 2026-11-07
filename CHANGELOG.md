# Changelog

All notable changes to this project will be documented in this file.

## [0.1.0] - 2026-11-08

### Added

- Initial release of `lease`: run one CLI tool in a disposable sandbox.
- Commands: `run`, `gc`, `status`, `doctor`, `version`, plus `--self-test` and `--json`.
- Sandboxing via `bwrap` inside `systemd-run --user --scope` (with a direct-`bwrap`
  fallback when no user systemd bus is available), with read-only `/usr`, a
  minimal `/etc`, read-only inputs at `/input/<n>`, writable `/out` and `/home`,
  no network by default and loopback-only with `--network localhost`.
- TTL enforcement, 10 MB stdout/stderr caps, `lease.json` per run, telemetry
  line on stderr, deterministic JSON output and documented exit codes.
- `plugin.toml` (`arabica-plugin-v1`), JSON schemas in `schema/`, fixtures in
  `fixtures/`, `Makefile`, and CI that runs unit tests, `--self-test`, and `doctor`.
