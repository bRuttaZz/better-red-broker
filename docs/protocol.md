# Protocol (v1)

Everything a client needs to talk to a broker. The reference client is the Better Red Android app
(`app/src/main/java/site/brutt/betterred/share/`).

## Conventions

- Base URL `https://<domain>`. The user enters only the domain; clients add the scheme and paths.
- JSON in and out; `Content-Type: application/json` on requests with a body.
- Binary values: **base64url without padding**. Times: unix seconds.
- Errors: `{"error": "<message>", "serverTime": <unix s>}` with a 4xx/5xx status.

## Values

| name | size | who has it | |
|---|---|---|---|
| device key | 32 B | her phone only | random, made once per install |
| device id | 16 B (22 chars) | inside the pair id | `SHA-256(deviceKey)[:16]` |
| pair id | 55 chars | both phones (QR) | `deviceId + "." + base64url(salt[8] ‖ HMAC-SHA256(serverKey, "better-red/pair/v1\0" ‖ deviceId ‖ "\0" ‖ salt)[:16])` |
| pair secret | 32 B | both phones (QR), broker (stored) | random, made by her phone per pairing |
| share public key | 65 B | her phone | P-256, uncompressed point |
| share private key | 32 B | partner (QR) | P-256 scalar |
| server key | ≥32 B | broker config | `server.key`; keep it to keep pair ids valid |

**One-time code**: RFC 6238 TOTP, HMAC-SHA256, 30-second steps, 8 digits, key = the pair secret.
The broker accepts the current step ±2. Test vector (RFC 6238 appendix B): key
`"12345678901234567890123456789012"` (ASCII), time 59 → `46119246`.
Device id vector: 32 zero bytes → `Zmh6rfhivXdsj8GLjp-OIA` (`vectors_test.go`, app `ShareCryptoTest`).

## Headers

| header | sent by | |
|---|---|---|
| `X-Better-Red-Pair` | both, on `/v1/days*` | the pair id |
| `X-Better-Red-Device` | her phone | her device key (base64url); its hash must equal the pair id's device part |
| `X-Better-Red-Code` | partner | the one-time code |
| `X-Better-Red-Password` | both, on every `/v1/*` call except `/v1/info` | only for a private broker (started with `BR_PASSWORD`); ignored by open brokers |

### Optional password (private, self-hosted brokers)

A broker started with `BR_PASSWORD` (or `-password-file`) refuses every API call (`POST /v1/pairs`
and `/v1/days*`) without the right `X-Better-Red-Password`: **401** with
`WWW-Authenticate: BetterRed-Password`, checked before anything else, counted as a failed
authentication (rate-limited). `GET /v1/info` stays public and says `"password": true`; the website
and `/healthz` stay public too. The password is 1–128 printable ASCII characters, no spaces.

In the app it's typed like a connection string, `password@domain` (the last `@` splits them), and
it travels in the QR code (the partner needs it too). The open default broker needs nothing.

## Endpoints

### `POST /v1/pairs`: mint a pair id (her phone)

Body `{"deviceKey": "<32 B>"}` (≤ 1 KB). Deletes every row `"<device-id>.*"`, **stores nothing**,
returns `201 {"pairId": "…", "maxDays": 60}`. `400` bad input, `429` more than 10 per hour from
this network.

### `PUT /v1/days`: write (her phone)

Headers: pair + device. Body (≤ 512 KB, no unknown fields):

```json
{
  "secret": "<32 B pair secret>",
  "put":    {"0": {"blob": "…", "updatedAt": 1795000000}, "2026-11-20": {"blob": "…", "updatedAt": 1794990000}},
  "delete": ["2026-11-02"]
}
```

Creates the row if missing, stores the secret as sent, applies deletes then puts, drops anything
outside the window, in one transaction. `200 {"ok": true, "updatedAt": …}`. Rules (`400` otherwise):

- keys: `"0"` or a real date `YYYY-MM-DD` within `[today − maxDays − 1, today + 1]` (UTC);
- at most `maxDays + 4` keys in `put` and in `delete`;
- `updatedAt` between 2020-01-01 and now + 1 day;
- blob: base64url, 95 B … 4 KB decoded, first byte `1`.

`503` when the server is full and this would be a new row.

### `GET /v1/days/index`: what's stored (her phone)

Headers: pair + device. `200 {"days": {"0": 1795000000, "2026-11-20": 1794990000}, "maxDays": 60}`.
No blobs. `{}` when nothing is stored: her phone then sends everything.

### `DELETE /v1/days`: unpair (her phone)

Headers: pair + device. `204`, also when nothing was stored.

### `GET /v1/days`: read (partner)

Headers: pair + code. `200 {"days": {"0": {"blob": "…", "updatedAt": …}, …}, "maxDays": 60, "updatedAt": …}`
(only keys inside the window). `404` when nothing is stored: the partner waits for her phone.

### Others

`GET /v1/info` → `{"name", "version", "maxDays", "staleDays", "serverTime", "password"}` (`password`: whether this broker needs one) · `GET /healthz` → `ok` ·
`GET /` → the landing page.

### Status codes on `/v1/days*`

| status | meaning | the app shows |
|---|---|---|
| 403 | pair id missing, malformed, or not signed by this server's key | "pair again" |
| 401 + `WWW-Authenticate: BetterRed-Password` | private broker: password missing or wrong (any `/v1` call but info) | "the server refused its password": pair again with the new one |
| 401 | her: device key doesn't match the pair id · partner: wrong or expired code | her: "pair again" · partner: "check the date and time" (after one retry with the clock taken from the `Date` header) |
| 404 | partner: nothing stored for this pair | "Waiting for her phone to send updates" |
| 429 | rate limited | "try again later" |

## Blobs

```
byte 0        version = 1
bytes 1–65    ephemeral P-256 public key, uncompressed (0x04 | X | Y)
bytes 66–77   AES-GCM nonce (random)
bytes 78…     AES-256-GCM ciphertext + 16-byte tag
```

- `shared = ECDH(ephemeral private, partner public)`
- `key = HKDF-SHA256(ikm = shared, salt = ephemeral public, info = "better-red/blob/v1")`, 32 bytes
- associated data `"<pair id>|<key>"` (`key` = `"0"` or the date): a blob can't be moved to another
  day or pair without the partner noticing.
- Receivers check the ephemeral point is on the curve.

Plaintext: one packing byte (`0` UTF-8 JSON as is, `1` raw DEFLATE, whichever is smaller), the
data, then zero bytes up to a multiple of 64. Receivers refuse to inflate past 64 KB. Her phone
sends every day of the window, `{}` for days with nothing shared, so all dates look alike.

### Settings blob `"0"`

```json
{"v": 1, "name": "Better", "cycle": 28, "period": 5, "luteal": 14, "days": 60,
 "share": {"mood": true, "symptoms": false, "notes": false, "fertility": false}}
```

### Day blob `"YYYY-MM-DD"`

The app's own `day_log.data` format, filtered to what she shares:
`{"flow": "MEDIUM", "mood": "CALM", "tags": ["CRAMPS"], "note": "…"}`. All keys optional; unknown
keys and values are ignored, so newer apps can add things without breaking older partners.

## QR code

`BR1|<server>|<pair id>|<share private key>|<pair secret>` (~180 characters, error correction M).
`<server>` is the domain, or `<password, URL-encoded>@<domain>` for a private broker (so `|` and
`@` in the password can't break the format; app versions before 1.2.0 refuse such codes).
Whoever has it can read what she shares; show it only on her screen.
