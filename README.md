# leaptel-api

A fake [Leaptel](https://leaptel.com.au) wholesaler API server, plus a Go
client for the real one.

If you build against Leaptel's wholesale API, this lets you develop and test
without touching production: point `LEAPTEL_BASE_URL` at this server and your
code talks to an in-memory fake that speaks the same `/api/v1/wholesaler/*`
endpoints. Orders, services, appointments and assurance tests all behave
plausibly, and nothing you do provisions a real customer.

Read-only endpoints that are safe to hit for real are proxied upstream with
your own credentials, so product catalogues and service qualifications return
live data.

## Install

```sh
go install github.com/the-it-dept/leaptel-api@latest
leaptel-api -addr :9091
```

Or run from source:

```sh
go run . -addr :9091
```

Flags:

| Flag | Default | Description |
| --- | --- | --- |
| `-addr` | `:9091` | Listen address |
| `-state-file` | `./leaptel-api.state.json` | Where to persist fake state; empty disables persistence |

`GET /health` returns `{"status":"ok","service":"fake-leaptel"}`.

## What is faked vs proxied

Requests carry the caller's own HTTP Basic credentials, exactly as the real
API expects.

**Proxied** — forwarded to `api.wholesaler.leaptel.com.au` using your
credentials. These are read-only, so they return real data:

- `GET /products`
- `POST /service-qualifications`
- `GET /service-assurance-tests`

**Faked** — served from in-memory state. No real provisioning happens:

- `POST /customers`, `GET /customers`, `GET /customers/{id}/services`
- `POST /orders`, `GET /orders/{id}`, `PATCH /orders/{id}/appointment`
- `GET /services/{id}`, `POST /services/{id}/modify`, `POST /services/{id}/cancel`
- `POST /services/{id}/assurance-tests`, `GET /services/{id}/assurance-tests/{testId}`, `GET /services/{id}/assurance-tests-history`
- `GET /appointments/time-slots`, `POST /appointments`

There is also a dev-only `POST /services/{id}/cease` with no upstream
equivalent, for simulating carrier-initiated churn.

Assurance tests return fixtures captured from real responses (see
`fixtures/`), so the payload shape matches what production returns —
including the quirks. Test numbers without a fixture fall back to a synthetic
green Service Health result. The fixtures are scrubbed: addresses, AVC/NTD
and serial numbers, and Leaptel service and test ids are synthetic values of
the same shape.

State persists to `-state-file` across restarts. Delete the file to reset.

## Using the client

The `leaptel` package is a plain Go client for the real API, usable on its
own:

```go
import "github.com/the-it-dept/leaptel-api/leaptel"

// Empty baseURL points at http://localhost:9091 — the fake, not production.
// Pass the wholesaler URL explicitly when you mean it.
c := leaptel.NewClient("", username, password)
```

Defaulting to the fake is deliberate: a forgotten config should fail loudly
with connection-refused, not quietly mis-provision a customer.

## About this repo

This is a read-only mirror. The source of truth is a private monorepo at
`the-it-dept/underlay`, and every push to `main` there that touches the fake or
the client syncs into this repo automatically. Commits here reference the
upstream revision they were built from.

Issues and discussion are welcome. Pull requests can't be merged here
directly — the next sync would overwrite them — but a PR is a fine way to
show a fix, and it'll be applied upstream and credited.

## License

MIT — see [LICENSE](LICENSE).
