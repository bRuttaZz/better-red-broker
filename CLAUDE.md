# better-red-broker — notes for Claude

The relay for Better Red's partner sharing (the Android app is in `../better-red`). Read
`docs/design.md` first; `docs/protocol.md` is the contract with the app.

Rules decided with the owner:

- **It must never be able to read user data.** Blobs are end-to-end encrypted on her phone; the
  private key travels only in the QR code. Never add anything that needs plaintext.
- **It's a dumb, disposable cache.** Minting a pair id stores nothing; the row appears on her first
  write (upsert, with the pair secret she sends every time); if the database is lost her phone
  refills it. Only `server.key` matters. Don't add state the phones can't recreate.
- One table `pair` (pair_id PK, data JSON, updated_at, publisher_secret). Keep it that simple.
- Pair ids are self-validating: `<device-id>.<base64url(salt ‖ HMAC(server key, device id ‖ salt))>`;
  they travel in the `X-Better-Red-Pair` header (never URLs) and are checked before any DB work.
  Her phone proves the device id with its device key (hashed, never stored); the partner reads with
  a TOTP from the pair secret. Minting a new pair id deletes the device's old `device-id.*` rows.
- Max 60 days per pair (`-max-days`), pairs unused for 10 days are deleted (`-stale-days`); the
  clean-up job runs hourly. Every read and write also enforces the window.
- Validate everything, cap every size, rate-limit per IP; it's public and easy to abuse
  (see docs/security.md). No IPs or pair ids in logs. Rate limits are in-memory token buckets
  (`-rate-limit=false` turns them off for proxies that limit); the client IP header
  (X-Forwarded-For by default) is believed only from `-trusted-proxies`, read from the right.
- SQLite: one writer connection, a read-only pool, WAL. Pragmas in `store.go`.
- Pure Go (modernc.org/sqlite, no cgo). Ask before adding any other dependency.
- The landing page (`web/`) uses the app's pastel red **light** theme only (no dark switch, no
  scripts); logo without the white circle; author and source link small and grey in the footer (the
  owner wrote that line: keep it); download button → GitHub releases; version shown.
- Container: `Containerfile` (distroless, non-root 65532); all state in the `/data` volume, marked
  with `VOLUME` and the `site.brutt.better-red.volume` label. `.containerignore` must keep `data/`,
  `*.db` and `*.key` out of images. A `v*` tag runs `.github/workflows/release.yml` (tests →
  binaries + GitHub release + ghcr.io image); the tag must equal `v` + the `VERSION` file, which
  is the only place the version lives (embedded in the binary).

Workflow: `gofmt -w . && go vet ./... && go test -race ./...` after every change. Changing the
protocol means changing the app's `share/` package and `docs/protocol.md` together, keeping old
clients working. The owner makes the commits: don't commit or push unless asked. Never commit
`data/`, `*.db` or `*.key`.
