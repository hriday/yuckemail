# yuckemail.online: throwaway inbox (design)

## Why
Sign up for sites you don't trust without handing them your real address.
It's for personal use first and could be shared with friends and family later.
It only receives mail and never sends any.

## What a visitor sees
1. They open the site and immediately get a random address such as `k7fq2x9m3a@yuckemail.online`, with a copy button.
2. A visible countdown starts at **05:00**. A **Renew** button resets it to 05:00 for the same address.
3. Switching tabs or apps does not matter. Only the clock does.
4. Below that is a box that fills live as mail arrives. Clicking a message shows it fully rendered, with images.
5. At 00:00 the address is burned. Mail sent to it afterwards is rejected. The page then offers a fresh address.

There are no accounts and no respawning. An address is never reused.

## Decisions
- **User's decisions:** receive-only; random addresses; 5-minute countdown with Renew; no login; no respawn;
  full HTML with images, rendered safely; attachments blocked (listed by name only); works on
  Safari, Chrome, and Edge, desktop and mobile; domain `yuckemail.online`; runs on a cheap VPS; written in Go.
- **Builder's calls (Claude):**
  - A single Go binary runs the SMTP listener, the HTTP site, and the API.
  - **Storage is in memory, not SQLite.** Nothing outlives 5 minutes, so there is no reason to touch disk:
    "nothing is kept" is automatic, it scales on tiny hardware, and a restart just drops live inboxes.
  - Each mailbox gets a secret browser token. Reading an inbox requires the token, so knowing the address alone
    (for example, the site you gave it to) is not enough to read the mail.
  - Live updates use Server-Sent Events, which every target browser supports, with a fallback refetch when the tab regains focus.
  - Mail safety: the HTML is sanitized, then served with a strict CSP (no scripts, no remote loads except our
    image proxy) inside a sandboxed iframe with no `allow-scripts` and no `allow-same-origin`. Links open in a new tab
    with `noopener noreferrer`.
  - Images go through a signed (HMAC) proxy that rejects private and internal IPs (SSRF), allows only `image/*`,
    caps size, sends no cookies or referer, and uses a generic user agent. The sender sees the VPS, never the visitor.
  - Limits: 10 MB per message, 50 messages per mailbox, rate-limited address creation per IP.

## Non-goals
Sending mail, accounts, history, attachment downloads, and spam filtering.

## Anti-goals
- A malicious email that can run code, read other inboxes, or reveal the visitor's IP.
- Someone other than the visitor who created a mailbox being able to read it.
- The image proxy becoming an open proxy into the VPS's network.
