# yuckemail architecture

```mermaid
flowchart LR
    sender([Sending mail server]) -- "SMTP :25" --> smtp

    subgraph bin [yuckemail binary]
        smtp[SMTP listener<br/>smtp.go]
        store[(In-memory store<br/>store.go)]
        http[HTTP server + API<br/>http.go]
        render[Sanitizer + renderer<br/>render.go]
        proxy[Signed image proxy<br/>/img]
        sweep{{Sweeper<br/>every 2s}}

        smtp -- "RCPT: IsLive?" --> store
        smtp -- "Deliver" --> store
        store -- "new message" --> http
        http --> render
        render -- "rewrites img src" --> proxy
        sweep -- "delete expired" --> store
    end

    caddy[Caddy<br/>TLS :443] --> http
    browser([Browser tab]) -- "HTTPS" --> caddy
    proxy -- "fetch, public IPs only" --> remote([Remote image hosts])
```

## Files
| File | Role |
|------|------|
| `main.go` | Flags, wiring, background sweeper and rate-limit pruning, graceful shutdown. |
| `smtp.go` | Accepts mail only for live addresses, parses MIME into a `Message`. |
| `store.go` | Mailboxes indexed by address and by token; delivery, renew, subscriptions, expiry. |
| `http.go` | Static site, JSON API, SSE stream, message rendering, per-IP rate limit. |
| `render.go` | HTML sanitizing, content CSP, HMAC-signed image proxy with SSRF guard. |
| `static/` | The single-page UI (embedded into the binary). |

## Lifecycle of a mailbox

```mermaid
sequenceDiagram
    participant B as Browser
    participant H as HTTP
    participant S as Store
    participant M as SMTP
    participant X as Sender

    B->>H: POST /api/mailbox
    H->>S: Create()
    S-->>B: address, token, expiresAt
    B->>H: GET /api/events (token)
    X->>M: RCPT TO address
    M->>S: IsLive?
    S-->>M: yes
    X->>M: DATA
    M->>S: Deliver
    S-->>B: SSE: new message
    B->>H: GET /m/{token}/{id}
    H-->>B: sanitized HTML (CSP, sandboxed iframe)
    B->>H: POST /api/mailbox/renew
    Note over S: expiresAt reached
    S->>S: Sweep deletes mailbox and mail
    X->>M: RCPT TO address
    M-->>X: 550 No such mailbox
```

## HTTP routes
| Route | Purpose |
|-------|---------|
| `POST /api/mailbox` | Create a mailbox (rate-limited). Returns address and token. |
| `GET /api/mailbox` | Snapshot of the mailbox for the bearer token. |
| `POST /api/mailbox/renew` | Reset the countdown. |
| `GET /api/events` | Server-Sent Events stream of new messages. |
| `GET /m/{token}/{id}` | Rendered message body for the iframe. |
| `GET /m/{token}/{id}/cid/{cid}` | Inline image parts. |
| `GET /img` | Signed remote-image proxy. |
| `GET /healthz` | Liveness and live mailbox count. |
