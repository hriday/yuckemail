# How yuckemail works

A walkthrough of the code, following one mailbox from creation to deletion. For the reasoning behind each
choice see [design.md](design.md). For diagrams and the route table see [architecture.md](architecture.md).

## Contents
1. [The process](#1-the-process)
2. [Creating a mailbox](#2-creating-a-mailbox)
3. [Receiving mail over SMTP](#3-receiving-mail-over-smtp)
4. [Live updates with Server-Sent Events](#4-live-updates-with-server-sent-events)
5. [The countdown, renewing, and expiry](#5-the-countdown-renewing-and-expiry)
6. [Rendering an email safely](#6-rendering-an-email-safely)
7. [The image proxy](#7-the-image-proxy)
8. [Rate limiting and client IPs](#8-rate-limiting-and-client-ips)
9. [Deployment](#9-deployment)
10. [Consequences of the design](#10-consequences-of-the-design)

## 1. The process

`main.go` builds the whole app in one process:

- A random 32-byte **HMAC key** for signing image-proxy URLs. It is never saved, so it changes on every restart.
- The **store** (`NewStore(ttl, 50)`), which keeps every mailbox in memory, with at most 50 messages each.
- The **renderer** and **image proxy**, which share that key.
- A **rate limiter** allowing 30 new mailboxes per IP every 10 minutes.
- Two background loops. `store.Sweep()` runs every 2 seconds and `limiter.Prune()` runs every minute.
- The **SMTP server** (default `:25`) and the **HTTP server** (default `:8080`), each in its own goroutine.

On SIGINT or SIGTERM, the HTTP server gets 5 seconds to shut down and the SMTP server is closed. All mail is
in memory, so stopping the process deletes it.

| Flag | Default | Meaning |
|------|---------|---------|
| `-domain` | `yuckemail.online` | Domain that addresses are issued on and accepted for |
| `-smtp` | `:25` | SMTP listen address |
| `-http` | `:8080` | HTTP listen address |
| `-ttl` | `5m` | Mailbox lifetime, and how long a renew lasts |
| `-max-bytes` | `10 MB` | Largest accepted message |
| `-trust-proxy` | `false` | Read the client IP from `X-Forwarded-For` |

## 2. Creating a mailbox

The page loads `static/app.js`. It first tries `resume()`: if `sessionStorage` holds a token, it calls
`GET /api/mailbox` with it. If there is no token or the mailbox is gone, it calls `POST /api/mailbox`.

`Store.Create()` (`store.go`) generates two random values with `crypto/rand`, encoded in lowercase base32:

| Value | Size | Visibility | Used by |
|-------|------|------------|---------|
| Address (local part) | 10 characters, about 50 bits | Public: you give it to websites | SMTP, through `byAddr` |
| Token | 32 characters, 160 bits | Secret: only this browser tab has it | HTTP, through `byToken` |

The store keeps one map for each, so SMTP looks mailboxes up by address and HTTP looks them up by token. The
two never mix: knowing an address doesn't let anyone read the inbox.

The response contains the full address, the token, `expiresAt`, and `serverNow`. The browser saves the token in
`sessionStorage`. That means a reload keeps the same inbox, but a new tab or window gets a new one. After that,
every API call sends `Authorization: Bearer <token>`.

## 3. Receiving mail over SMTP

`smtp.go` uses `emersion/go-smtp` and only receives mail. It never sends or relays anything.

**`RCPT TO`** (`smtpSession.Rcpt`) is where mail is accepted or refused:
- Recipients on another domain get `550 5.7.1 Relaying denied`.
- Recipients that don't exist or have expired get `550 5.1.1 No such mailbox`, decided by `Store.IsLive`.
- Accepted recipients are kept for the session, up to 20.

Mail for burned or made-up addresses is refused here, before its content is ever sent to us.

**`DATA`** (`smtpSession.Data`):
1. Reads the whole message, up to `-max-bytes`.
2. Parses it with `enmime`, once per recipient, so each mailbox gets its own copy and nothing is shared.
3. `parseMessage` keeps:
   - `From` and `Subject`, with `(no subject)` used when the subject is empty
   - the plain text and HTML bodies
   - inline images that have a `Content-ID`, stored by that ID so `cid:` links work
   - attachment **file names only**, since attachments can't be downloaded

   The raw message is thrown away.
4. `Store.Deliver` gives the message a random ID, appends it, and sends it to every open browser stream. If the
   mailbox already has 50 messages, the new one is dropped. If no recipient received it, the sender gets a 550.

The server sets 30-second read and write timeouts.

## 4. Live updates with Server-Sent Events

The browser opens `EventSource("/api/events?token=…")`. `EventSource` can't send custom headers, so the token
goes in the query string. That is safe because the site is served only over HTTPS in production.

On the server (`App.events`):
- `Store.Subscribe` registers a channel that holds up to 16 messages.
- It sends `retry: 3000` so the browser reconnects after 3 seconds if the connection drops.
- Each new message goes out as `event: mail` with the message JSON as data. The HTML body and inline images
  aren't included. The browser loads those separately when the message is opened.
- A `: ping` comment every 20 seconds keeps proxies and NATs from closing an idle connection.
- When the sweep closes the channel, the server sends `event: expired` and ends the stream.
- `X-Accel-Buffering: no` and a flush after every event make sure nothing sits in a buffer.

`Deliver` never waits on a slow browser. If a channel is full, that browser misses the push, but it gets the
message the next time it refetches.

On the client, catching up covers everything a stream can miss:
- On a `visibilitychange` back to visible, or a `pageshow` from the back/forward cache, the page calls
  `refresh()`, which fetches `GET /api/mailbox` again. Mobile browsers suspend background tabs, so this matters.
- If the stream closes for good (for example, the server answers 410), the page calls `refresh()` to check
  whether the mailbox still exists.
- A `seen` set of message IDs prevents duplicates when pushes and refetches overlap.

## 5. The countdown, renewing, and expiry

**Clock.** The server decides when a mailbox expires. Each response includes `expiresAt` and `serverNow`. The
client computes `skew = serverNow - Date.now()` and adjusts the expiry by it, so the countdown is right even if
the device clock is wrong. `tick()` redraws the timer every 250 ms and turns it red in the last minute.

**Renewing.** `POST /api/mailbox/renew` runs `Store.Renew`, which sets `Expires = now + ttl`. That is a reset,
not an addition: renewing twice in a row still leaves 5 minutes.

**Expiry** happens in three places:
1. **SMTP:** `IsLive` and `Deliver` compare against `Expires` every time, so mail is refused the moment the
   mailbox expires, even before the sweep runs.
2. **HTTP:** every token lookup goes through `liveByToken`, so an expired token gets `410 Gone`.
3. **Sweep (every 2 seconds):** deletes the mailbox from both maps, drops its messages, and closes its browser
   streams, which then receive `event: expired`.

When the timer reaches zero or the server reports the mailbox is gone, the client calls `burn()`. That clears the
token and the message list, shows "burned", and offers a new address.

## 6. Rendering an email safely

Clicking a message creates an iframe pointing at `/m/{token}/{id}`. Email HTML can't be trusted, so it passes
through four separate layers. Any one of them is enough to stop scripts from running.

**Layer 1: sanitizing** (`Renderer.Sanitize`, `render.go`). This uses bluemonday's `UGCPolicy`, plus the elements
and attributes email layouts depend on: tables, `font`, `center`, `bgcolor`, inline `style`, and `<style>`
blocks. Everything else, including scripts, event handlers, forms, and embedded objects, is removed. Links get
`rel="noreferrer"` and absolute links get `target="_blank"`.

**Layer 2: rewriting image URLs.** Every `src` is rewritten:

| Original | Rewritten to |
|----------|--------------|
| `cid:abc` | `/m/{token}/{id}/cid/abc`, an inline image from the same message |
| `http(s)://…` | `/img?u=…&s=<hmac>`, through the signed proxy |
| `data:` image | left as is |
| anything else | `/blocked`, which returns 204 with no content |

**Layer 3: the content security policy.** The message response is served with:

```
default-src 'none'; img-src 'self' <origin> data:; style-src 'unsafe-inline';
base-uri 'none'; form-action 'none'; frame-ancestors 'self';
sandbox allow-popups allow-popups-to-escape-sandbox
```

So the message can't run scripts, load remote fonts or styles, submit forms, or be framed by another site.
`<origin>` is listed explicitly because a sandboxed document has an opaque origin, which `'self'` wouldn't
match. Without it, proxied images wouldn't load.

**Layer 4: the sandboxed iframe.** `app.js` creates the iframe with
`sandbox="allow-popups allow-popups-to-escape-sandbox"`, with no `allow-scripts` and no `allow-same-origin`,
plus `referrerpolicy="no-referrer"`. Even if something got past layers 1 to 3, it couldn't run code or read
the parent page.

Plain-text messages are HTML-escaped and wrapped in a `<pre>`. Inline `cid:` images are served only if their
content type is a raster image, with `nosniff` and a `sandbox` CSP. The main app pages get their own strict
CSP from `withAppHeaders`.

## 7. The image proxy

`GET /img?u=<url>&s=<sig>` (`imageProxy.ServeHTTP`):

1. **Signature check.** `s` must be the first 16 bytes of `HMAC-SHA256(key, u)`, compared in constant time. The
   server only signs URLs that appeared in mail it rendered, so `/img` can't be used as an open proxy. Because
   the key changes on restart, old signed URLs stop working.
2. **URL check.** Only `http` and `https` URLs are fetched.
3. **Private networks blocked.** The dialer's `Control` hook (`publicOnly`) runs after DNS resolution, on the
   actual IP being connected to. It refuses loopback, private, link-local, unspecified, and CGNAT (100.64/10)
   addresses. Because it checks the resolved IP, a hostname that points at an internal address is refused too.
   Every redirect goes through the same dialer, with at most 3 redirects and only to `http` or `https`.
4. **Privacy.** The request uses a generic user agent and sends no cookies or referrer, so the sender never
   learns anything about the visitor.
5. **Content checks.** The response must be a 200 with a raster image type (png, jpeg, gif, webp, avif, bmp, or
   ico). SVG is refused because it can contain scripts. Images are limited to 5 MB, checked against both
   `Content-Length` and the bytes actually read.
6. **Response headers.** `nosniff`, `default-src 'none'; sandbox`, and `Cache-Control: private, max-age=300`.

Timeouts: 5 seconds to connect, 8 seconds for headers, 10 seconds in total.

## 8. Rate limiting and client IPs

`rateLimiter` keeps a list of recent creation times for each IP and allows 30 in any 10-minute window. Only
`POST /api/mailbox` is limited. Anything over the limit gets a `429` with a message the UI shows.

`clientIP` normally uses the TCP peer address. With `-trust-proxy`, it uses the **last** entry in
`X-Forwarded-For`, which is the one Caddy appended. Earlier entries are ignored because the client can set them
to anything.

## 9. Deployment

```
Internet ──:25──────────────────────────────▶ yuckemail (SMTP)
Internet ──:443──▶ Caddy (TLS, automatic certs) ──▶ 127.0.0.1:8080 yuckemail (HTTP)
```

- **DNS:** `MX yuckemail.online → mail.yuckemail.online`, plus A records for both pointing at the server.
- **Caddy** (`deploy/Caddyfile`) gets HTTPS certificates automatically and forwards requests. Don't turn on
  `encode` (compression) for this site: it buffers responses, which delays the live inbox stream.
- **systemd** (`deploy/yuckemail.service`) runs the binary as an unprivileged `yuckemail` user. It can still
  bind port 25 through `AmbientCapabilities=CAP_NET_BIND_SERVICE`. The unit also sets `NoNewPrivileges`,
  `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, and `PrivateDevices`.
- **SMTP has no TLS.** The server doesn't offer STARTTLS, so sending servers deliver over plain SMTP.

The README covers step-by-step deployment, including running behind a Caddy container that already exists.

## 10. Consequences of the design

- **A restart deletes every live inbox.** Each deploy loses at most a few minutes of mail. Old image-proxy
  links also stop working, because the HMAC key changes.
- **It can only run as one instance.** All state lives in one process's memory, so a second instance behind a
  load balancer wouldn't see the first one's mailboxes. Scaling out would require shared state.
- **Memory is the limit.** Roughly the worst case is live mailboxes × 50 messages × 10 MB. In practice parsed
  messages are much smaller and mailboxes last 5 minutes. Watch `/healthz`, which reports the live mailbox count.
- **Mail is gone when the mailbox expires.** Nothing is written to disk, logged, or backed up. That is the point
  of the app.
