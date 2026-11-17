# yuckemail design

## Purpose
Sign up for sites you don't trust without handing them your real address. yuckemail only receives mail; it
never sends any.

## What a visitor sees
1. Opening the site immediately issues a random address such as `k7fq2x9m3a@yuckemail.online`, with a copy button.
2. A countdown starts at **05:00**. **Renew** resets it to 05:00 for the same address.
3. Switching tabs or apps doesn't matter. Only the clock does.
4. Mail appears live in the inbox below. Clicking a message shows it fully rendered, images included.
5. At 00:00 the address is burned. Mail sent to it afterwards is rejected, and the page offers a fresh address.

There are no accounts, and an address is never reused.

## Decisions

**One binary.** A single Go process runs the SMTP listener, the website, and the API. There is nothing else
to deploy, configure, or keep in sync.

**Memory, not a database.** Nothing outlives five minutes, so nothing touches disk. "Nothing is kept" is a
property of the design rather than a cleanup job, it runs comfortably on tiny hardware, and a restart simply
drops live inboxes.

**Address and token are separate secrets.** Each mailbox has a public address and a private token held only by
the browser tab that created it. Reading mail requires the token, so the site you gave the address to can't
read the inbox.

**Reject at the door.** The SMTP server checks the recipient during `RCPT TO` and answers 550 for anything not
currently live. Mail for burned or made-up addresses is never accepted, parsed, or stored.

**Server-Sent Events for live updates.** Supported by every target browser (Safari, Chrome, Edge, desktop and
mobile) with no extra protocol. The page also refetches when the tab regains focus, which covers mobile
browsers that suspend background connections.

**The server owns the clock.** Responses carry `expiresAt` and `serverNow`, so the countdown is correct even when
the visitor's device clock is wrong.

## Rendering untrusted mail
Email HTML is hostile input. It passes through several independent layers, and any one of them is enough to
stop scripts:

1. **Sanitize.** HTML is cleaned with an allowlist (bluemonday). Image sources are rewritten: `cid:` goes to the
   message's inline parts, `http(s):` goes to the signed image proxy, and anything else goes to a dead endpoint.
2. **CSP.** The rendered message is served with `default-src 'none'`, no scripts, no frames, no forms, and images
   only from our own origin.
3. **Sandbox.** It is shown in an iframe without `allow-scripts` or `allow-same-origin`. Links open in a new tab
   with `noopener noreferrer`.

## Image proxy
Remote images load through `/img` so senders never learn the visitor's IP or see a referer.

- URLs are HMAC-signed by the server, so `/img` is not an open proxy.
- Connections to private, loopback, and link-local addresses are refused at dial time (SSRF).
- Only raster image types are passed through (no SVG), capped at 5 MB.
- No cookies or referer are sent, and the user agent is generic.

## Limits
- 10 MB per message, 50 messages per mailbox.
- 30 new addresses per IP per 10 minutes.
- Attachments are listed by name only and can't be downloaded.

## Non-goals
Sending mail, accounts, history, attachment downloads, and spam filtering.

## Must never happen
- A malicious email runs code, reads other inboxes, or reveals the visitor's IP.
- Anyone other than the visitor who created a mailbox can read it.
- The image proxy becomes a way into the server's own network.
