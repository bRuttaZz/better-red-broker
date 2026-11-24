# Development

## Code layout

| file | |
|---|---|
| `main.go` | start-up, HTTP server and its limits, middleware (security headers, rate limit, logging), clean-up schedule |
| `config.go` | flags / environment variables, `version` (from `VERSION`) |
| `VERSION` | the broker's version, one line; embedded in the binary |
| `auth.go` | device ids, salted pair id signing, TOTP, the server key file |
| `api.go` | `/v1` handlers and every validation rule |
| `store.go` | SQLite: schema, connection pools, pragmas, queries, clean-up |
| `ratelimit.go` | in-memory per-IP token buckets; the client IP behind trusted proxies (X-Forwarded-For read from the right) |
| `site.go`, `web/` | the landing page (embedded in the binary); the app's light theme, no scripts |
| `Containerfile`, `.containerignore` | the container image (`.dockerignore` links to the ignore file) |
| `.github/workflows/release.yml` | tag `v*` → tests, binaries, GitHub release, image |
| `*_test.go` | tests; `vectors_test.go` holds values shared with the Android tests |

## Tests

```bash
go vet ./... && go test -race ./...
```

They cover TOTP (RFC vectors), the device id (vector shared with the app), salted pair ids and
forgery, the full mint → write → index → read → re-pair → delete flow, refilling after the
database is lost, every validation rule, size limits, the wrong-code limiter, the clean-up job and
the website.

### End to end with the app

```bash
go run . -addr localhost:8099 -db /tmp/br/br.db -key-file /tmp/br/br.key
# in ../better-red:
BR_TEST_BROKER=localhost:8099 ./gradlew testDebugUnitTest --tests '*BrokerIntegrationTest*'
```

On devices: debug builds of the app accept `localhost:<port>` (and `10.0.2.2:<port>` from the
emulator) as a server, over plain http. Forward the port with `adb reverse tcp:8080 tcp:8080`, then
type `localhost:8080` as the server in the Partner tab.

## Releasing

`.github/workflows/release.yml` runs when a tag starting with `v` is pushed:

```bash
echo 0.2.0 > VERSION && git commit -S -am "Release 0.2.0"
git tag -s v0.2.0 -m "v0.2.0" && git push origin main v0.2.0
```

The version lives only in `VERSION`: the binary embeds it, and it's shown by `better-red-broker
-version`, in `GET /v1/info` (`"version"`), on the website ("Server version …") and in the start-up
log. The workflow refuses a tag that isn't `v` + `VERSION`.

1. **test**: gofmt, `go vet`, `go test -race`. Nothing is published if this fails.
2. **binaries**: static linux amd64/arm64 builds, `SHA256SUMS`, and a GitHub release with generated notes.
   Tags with a `-` (`v0.2.0-rc1`) become pre-releases.
3. **image**: multi-arch (amd64, arm64) image from `Containerfile`, pushed to
   `ghcr.io/<owner>/better-red-broker` as `0.2.0` and `0.2`, plus `latest` for final releases.
   The first time, make the package public in GitHub → Packages if it should be pullable without
   logging in.

## Changing the protocol

Keep old clients working: the app and the broker are updated separately, and phones update late.
New optional JSON fields are fine (the broker refuses unknown fields in requests, so add them to
the broker first). Anything incompatible goes under `/v2`, and the blob version byte changes.
Update docs/protocol.md and the app's `share/` package together.
