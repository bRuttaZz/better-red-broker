# better-red-broker

> ⚠️ Vibe Coded

The small relay behind [Better Red](https://github.com/bRuttaZz/better-red)'s partner sharing.

Better Red is a private period tracker. Partner sharing lets her partner see (never change) what
she chooses to share. This server passes that data between the two phones **without being able to
read it**: everything is end-to-end encrypted on her phone, and the key travels only through a QR
code shown on her screen.

- A dumb, disposable cache: it signs pair ids without storing them, keeps only what her phone
  writes, and if it loses everything her phone simply writes it again.
- Go, one binary, one SQLite file. One dependency: `modernc.org/sqlite` (pure Go, no cgo).
- Stores at most 60 days per pair; pairs unused for 10 days are deleted. Both configurable.
- No accounts, no emails, no IP addresses or pair ids in the logs.
- Serves a small landing page at `/` (logo, download button, version, credits; the app's light theme).

## Run it

```bash
go run .                                  # listens on :8080, data in ./data/
go build -o better-red-broker .            # version: the VERSION file
./better-red-broker -addr :8080 -db /var/lib/better-red/broker.db -key-file /var/lib/better-red/server.key
```

Or as a container (the image keeps everything in the `/data` volume):

```bash
podman run -d --name better-red-broker -p 127.0.0.1:8080:8080 \
  -v better-red-data:/data ghcr.io/bruttazz/better-red-broker:latest
```

Releases: pushing a `v*` tag builds binaries, a GitHub release and the image
([docs/development.md](docs/development.md#releasing)).

Put a reverse proxy in front for TLS (Caddy, nginx, Cloudflare Tunnel). The broker rate-limits per
client IP and takes it from `X-Forwarded-For` when the request comes from a trusted proxy
(`-trusted-proxies`, default `loopback`); or turn its limits off with `-rate-limit=false` and limit
at the proxy. See [docs/operations.md](docs/operations.md#behind-a-reverse-proxy).

## Test

```bash
go test -race ./...
```

The Android app has a matching end-to-end test (`BrokerIntegrationTest`) that runs against a live
broker: see [docs/development.md](docs/development.md).

## Documentation

- [docs/design.md](docs/design.md): what it does and why, the whole flow, the ideas behind it
- [docs/protocol.md](docs/protocol.md): HTTP API, formats, cryptography (for client authors)
- [docs/security.md](docs/security.md): threat model, what the server can and can't learn, abuse limits
- [docs/operations.md](docs/operations.md): settings, deploying (container or systemd), Cloudflare, backups, the database
- [docs/development.md](docs/development.md): code layout, tests, releasing

## License

GNU GPL v3, like the app. See [LICENSE](LICENSE).
