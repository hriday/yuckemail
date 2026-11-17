# yuckemail

A throwaway, receive-only inbox at `@yuckemail.online`. Open the site to get a random address and a
5:00 countdown. Mail appears live, **Renew** resets the clock, and at 00:00 the address and all its mail are gone.
See `docs/superpowers/specs/2026-11-16-yuckemail-design.md` for the design.

One Go binary runs the SMTP listener (port 25), the website, and the API. Everything lives in memory.

## Run locally

```sh
go test -race ./...
go run . -smtp 127.0.0.1:2525 -http 127.0.0.1:8088
# open http://localhost:8088, then send mail to the shown address via 127.0.0.1:2525
```

Flags: `-domain`, `-smtp`, `-http`, `-ttl` (default 5m), `-max-bytes` (default 10 MB), `-trust-proxy`.

## Deploy (cheap VPS, Ubuntu/Debian)

**0. Check port 25.** Confirm your provider allows *inbound* TCP 25. Most do; some (for example new
Hetzner or DigitalOcean accounts) restrict outbound 25 only, which doesn't matter for us.

**1. DNS at your registrar** (replace `203.0.113.10` with the VPS IP):

| Type | Name | Value |
|------|------|-------|
| A    | `yuckemail.online`      | `203.0.113.10` |
| A    | `mail.yuckemail.online` | `203.0.113.10` |
| MX   | `yuckemail.online`      | `mail.yuckemail.online` (priority 10) |

Also set the VPS's reverse DNS (PTR) to `mail.yuckemail.online` in the provider panel. Receiving works
without it, but some senders like it.

**2. Build and copy:**

```sh
GOOS=linux GOARCH=amd64 go build -o bin/yuckemail-linux .   # arm64 VPS: GOARCH=arm64
scp bin/yuckemail-linux root@VPS:/usr/local/bin/yuckemail
scp deploy/yuckemail.service root@VPS:/etc/systemd/system/
```

**3. On the VPS:**

```sh
useradd --system --no-create-home yuckemail
apt install -y caddy
cp Caddyfile /etc/caddy/Caddyfile        # copy deploy/Caddyfile up first
systemctl daemon-reload && systemctl enable --now yuckemail && systemctl reload caddy
ufw allow 25,80,443/tcp                   # if ufw is on
```

**4. Smoke test:** open https://yuckemail.online, then send a mail from Gmail to the address shown.
It should appear within seconds. Open it and confirm images load (via `/img`). Send to a made-up
address and you should get a bounce (550).

Updating later is just re-copying the binary and running `systemctl restart yuckemail`. That drops live inboxes, which is fine
because they last 5 minutes.

## Safety model
- Mail is sanitized, then served with a strict CSP (no scripts, frames, or forms) in a sandboxed iframe without
  `allow-scripts` or `allow-same-origin`.
- Remote images go through `/img`, which is HMAC-signed (not an open proxy), refuses private and internal IPs,
  allows only raster image types (no SVG), caps size at 5 MB, and sends no cookies or referer.
- Attachments are listed by name only and can't be downloaded.
- Reading a mailbox requires a secret per-tab token, so knowing the address alone isn't enough.
- Address creation is rate-limited to 30 per IP per 10 minutes.

## Running behind an existing Caddy container

If another project's Caddy (in Docker) already owns ports 80 and 443, run yuckemail as a host service with
`-http <docker-network-gateway>:8080 -trust-proxy`, then add a `yuckemail.online { reverse_proxy <gateway>:8080 }`
block to that Caddyfile. Leave `encode` off that block, because compression buffers the live inbox stream.
If the Caddyfile is a single-file bind mount, edit it **in place**. Tools that replace the file (`sed -i`,
many editors, `git checkout`) leave the container reading the old copy until the container restarts.
Apply changes with `caddy reload`.

## License

MIT. See [LICENSE](LICENSE).
