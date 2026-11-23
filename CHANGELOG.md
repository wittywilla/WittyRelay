# Changelog

## 0.2.0

### New

- **An admin page** for whoever runs a relay ([docs/ADMIN.md](docs/ADMIN.md)). Off unless `WITTYRELAY_ADMIN` is set; the Compose file turns it on at `http://localhost:8081`, kept to the machine itself.
  - **Status:** up or not, version, uptime, servers registered, device connections open and a minute, bandwidth now and at its peak, a ten-minute graph, and refusals by reason. Counts only, and short IDs that are new every run: never who uses the relay or what it carries.
  - **Health:** checks, the public addresses and ports, and the last 50 warnings and errors with nothing but their message.
  - **Settings:** bandwidth caps, connection and per-address limits, the name and note, and the log level, saved to the configuration file (`wittyrelay.json` in the data volume when there is no other). Settings set in the environment show read-only. Limits apply at the next restart, the log level at once.
  - **Connect a server:** ports to open, keys, where to paste the address on a WittyMediaServer, and a `docker-compose.yml`.
  - The first password is set with a one-time setup token from the log, or with `wittyrelay admin-password`. It's kept as an argon2id hash. Sign-in is rate-limited; sessions are `Secure`, `HttpOnly`, `SameSite=Strict` cookies; every form has a CSRF token; the Content-Security-Policy allows only the page's own embedded files.
  - Light and dark, following the device, in WittyMediaServer's colours; fits a phone; labelled for screen readers and usable from the keyboard.
- `wittyrelay admin-password` sets the admin page's password from standard input.
- The configuration file can also be `wittyrelay.json` in the data directory.

### Changed

- A `logLevel` that isn't `debug`, `info`, `warn` or `error` now stops the relay at start, with the reason, instead of being taken as `info`.

### Tests

- Every admin endpoint's sign-in and CSRF rules, sessions, rate limits, settings, and that no page shows a key, token or address (`go test -race`).
- The page in a real browser (Playwright, `e2e/`): first run, every page, settings, sign-out and in, the keyboard, a phone's width, dark mode, and no CSP violations.
- The image run as the Compose file runs it (read-only, no capabilities, user 65532), from the setup token to a setting that outlives a restart (`scripts/docker-admin-check.sh`).

## 0.1.0

- The first WittyRelay: libp2p's circuit relay v2 with registration, access keys, limits told before use, and speed limits, in a distroless image for amd64 and arm64.
