# nats-guest-access

`nats-guest-access` gives anonymous browser guests temporary, restricted access
to a NATS-backed application. It was created for interactive WhenGas audience
sessions, while keeping the session and token model application-neutral.

It is a small JWT issuer rather than a general identity provider. Guests do not
have accounts, passwords, passkeys, profiles, or an OAuth login flow. A guest
opens an opaque invitation URL, receives a numbered concurrent-capacity lease,
and gets an ES256 JWT that an application API and
[`nats-iam-broker`](https://github.com/jr200-labs/nats-iam-broker) can verify.

## Included

- NATS KV persistence with compare-and-set slot allocation
- presenter authentication through an existing OIDC provider and required group
- multiple presenter-owned events and capability-scoped cohosts
- opaque 256-bit invitation URLs suitable for QR codes
- configurable concurrent capacity with a global zero-value kill switch
- short-lived ES256 guest JWTs, OIDC discovery metadata, and JWKS
- heartbeat, five-minute reconnect grace, pause, and reserved-session resume
- purge-before-reuse through an application-owned NATS cleanup contract
- tracked event deletion with retry-stable deletion IDs and minimal receipts
- non-root container image

## Protocol surface

The verifier-facing endpoints are deliberately minimal:

| Endpoint | Purpose |
| --- | --- |
| `GET /.well-known/openid-configuration` | Advertise issuer and JWKS metadata |
| `GET /jwks` | Publish the ES256 public signing key |

There is no authorization, OAuth token, refresh-token, consent, client-registry,
or UserInfo endpoint. Browser sessions use:

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/guest-sessions` | Exchange `{"invitation":"…"}` or resume using the cookie |
| `POST /v1/guest-sessions/token` | Renew the lease and five-minute JWT |
| `POST /v1/guest-sessions/heartbeat` | Renew the lease without returning a new JWT |
| `DELETE /v1/guest-sessions/current` | Clear the browser's resume capability |

The invitation is carried in a URL fragment, for example
`https://www.whengas.com/demo#<secret>`. The browser sends the fragment in the
JSON request body. Fragments do not reach HTTP access logs or referrer headers.

Presenter endpoints require an OIDC bearer token containing the configured
presenter group:

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/events` | Create an event and return its invitation URL |
| `GET /v1/events` | List events owned or cohosted by the presenter |
| `GET /v1/events/{id}` | Read an event |
| `POST /v1/events/{id}/open` | Open a draft event |
| `POST /v1/events/{id}/pause` | Stop admission and token renewal; reserve slots |
| `POST /v1/events/{id}/reopen` | Reopen a paused event |
| `PUT /v1/events/{id}/capacity` | Change concurrent capacity |
| `POST /v1/events/{id}/invitation` | Rotate the invitation |
| `PUT /v1/events/{id}/cohosts` | Set a cohost's capabilities |
| `GET /v1/events/{id}/slots` | Inspect slot metadata |
| `DELETE /v1/events/{id}/slots/{number}` | Purge and release a slot |
| `POST /v1/events/{id}/delete` | Purge and permanently delete the event |

## Run

The service needs NATS with JetStream enabled, an ECDSA P-256 signing key, and a
high-entropy HMAC key.

```bash
go build -o bin/nats-guest-access ./cmd/nats-guest-access
bin/nats-guest-access keygen signing-key.pem

export NGA_ISSUER_URL=http://localhost:8080
export NGA_SIGNING_KEY_FILE=signing-key.pem
export NGA_INVITATION_HMAC_KEY="$(openssl rand -hex 32)"
export NGA_SECURE_COOKIES=false
bin/nats-guest-access
```

Management endpoints are disabled until `NGA_PRESENTER_ISSUER_URL` and
`NGA_PRESENTER_CLIENT_ID` are configured. Permanent deletion and expired-slot
reuse fail closed until `NGA_CLEANUP_SUBJECT` is configured and an application
cleanup responder is available.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `NGA_LISTEN_ADDRESS` | `:8080` | HTTP listen address |
| `NGA_ISSUER_URL` | required | Externally visible issuer URL |
| `NGA_AUDIENCE` | `whengas-demo` | Audience required by API and IAM verifiers |
| `NGA_PUBLIC_DEMO_URL` | `https://www.whengas.com/demo` | Base URL for invitation fragments |
| `NGA_SIGNING_KEY_FILE` | required | ECDSA P-256 private key in PEM format |
| `NGA_INVITATION_HMAC_KEY` | required | HMAC key of at least 32 characters |
| `NGA_TOKEN_TTL` | `5m` | Guest JWT lifetime; cannot exceed five minutes |
| `NGA_LEASE_TTL` | `30s` | Active lease duration between heartbeats |
| `NGA_GRACE_PERIOD` | `5m` | Reconnect period after the lease expires |
| `NGA_MAX_CAPACITY` | `100` | Per-event ceiling; zero disables admission |
| `NGA_NATS_URL` | `nats://127.0.0.1:4222` | NATS server URL |
| `NGA_NATS_CREDS_FILE` | empty | Optional NATS credentials file |
| `NGA_KV_BUCKET` | `NATS_GUEST_ACCESS` | State bucket |
| `NGA_CLEANUP_SUBJECT` | empty | Cleanup subject prefix; application ID is appended |
| `NGA_CLEANUP_TIMEOUT` | `30s` | Per-cleanup request timeout |
| `NGA_PRESENTER_ISSUER_URL` | empty | Pocket ID issuer used for management requests |
| `NGA_PRESENTER_CLIENT_ID` | empty | Expected presenter-token audience |
| `NGA_PRESENTER_GROUP` | `demo-presenters` | Group required for management access |
| `NGA_SECURE_COOKIES` | `true` | Add `Secure` to resume cookies |

## Cleanup contract

For `NGA_CLEANUP_SUBJECT=guest.cleanup`, cleanup requests are sent to
`guest.cleanup.<application_id>`. A request includes a stable `deletion_id`, the
event and application IDs, affected guest subjects, and a reason. The responder
must be idempotent by deletion ID and return:

```json
{
  "schema_version": 1,
  "deletion_id": "same-as-request",
  "application_id": "whengas",
  "status": "complete",
  "identities_processed": 12
}
```

The service does not release or reuse a slot until cleanup succeeds.

## Development

```bash
make lint
make test
make build
```
