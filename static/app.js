"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const el = {
    address: $("address"), copy: $("copy"), timer: $("timer"), renew: $("renew"),
    expired: $("expired"), fresh: $("fresh"), error: $("error"),
    live: $("live"), list: $("messages"), empty: $("empty"),
  };

  let token = null;
  let expiresAt = 0;   // ms, in local clock terms
  let es = null;
  let dead = false;
  const seen = new Set();

  // Keep the mailbox across a reload of this tab only (never shared across tabs).
  const saved = {
    get() { try { return sessionStorage.getItem("yuck.token"); } catch { return null; } },
    set(v) { try { v ? sessionStorage.setItem("yuck.token", v) : sessionStorage.removeItem("yuck.token"); } catch {} },
  };

  function setExpiry(data) {
    // Correct for client clock skew using the server's "now".
    const skew = Date.parse(data.serverNow) - Date.now();
    expiresAt = Date.parse(data.expiresAt) - skew;
  }

  function showError(msg) {
    el.error.textContent = msg || "";
    el.error.hidden = !msg;
  }

  async function api(method, path) {
    const res = await fetch(path, {
      method,
      headers: token ? { Authorization: "Bearer " + token } : {},
      cache: "no-store",
    });
    const body = await res.json().catch(() => ({}));
    return { status: res.status, body };
  }

  async function createMailbox() {
    showError("");
    const { status, body } = await api("POST", "/api/mailbox");
    if (status !== 201) { showError(body.error || "Couldn't create an address. Try again."); return false; }
    token = body.token; saved.set(token);
    load(body);
    return true;
  }

  async function resume() {
    token = saved.get();
    if (!token) return false;
    const { status, body } = await api("GET", "/api/mailbox");
    if (status !== 200) { token = null; saved.set(null); return false; }
    load(body);
    return true;
  }

  function load(data) {
    dead = false;
    seen.clear();
    el.list.textContent = "";
    el.address.textContent = data.address;
    el.expired.hidden = true;
    el.renew.disabled = false;
    el.copy.disabled = false;
    setExpiry(data);
    (data.messages || []).forEach((m) => addMessage(m, false));
    updateEmpty();
    connect();
    tick();
  }

  function connect() {
    if (es) es.close();
    if (!token || dead) return;
    es = new EventSource("/api/events?token=" + encodeURIComponent(token));
    es.addEventListener("open", () => setLive(true));
    es.addEventListener("mail", (e) => { addMessage(JSON.parse(e.data), true); updateEmpty(); });
    es.addEventListener("expired", () => burn());
    es.addEventListener("error", () => {
      setLive(false);
      // EventSource gives up on a non-200 (mailbox gone); confirm with the API.
      if (es && es.readyState === EventSource.CLOSED) refresh();
    });
  }

  function setLive(on) {
    el.live.classList.toggle("on", on);
    el.live.textContent = on ? "● live" : "● reconnecting";
  }

  async function refresh() {
    if (!token || dead) return;
    const { status, body } = await api("GET", "/api/mailbox");
    if (status !== 200) { burn(); return; }
    setExpiry(body);
    body.messages.forEach((m) => addMessage(m, true));
    updateEmpty();
    if (!es || es.readyState === EventSource.CLOSED) connect();
  }

  function burn() {
    if (dead) return;
    dead = true;
    if (es) { es.close(); es = null; }
    token = null; saved.set(null);
    expiresAt = 0;
    el.timer.textContent = "00:00";
    el.timer.classList.add("low");
    el.renew.disabled = true;
    el.copy.disabled = true;
    el.address.textContent = "burned";
    el.list.textContent = "";
    seen.clear();
    updateEmpty();
    el.live.classList.remove("on");
    el.live.textContent = "● off";
    el.expired.hidden = false;
  }

  function tick() {
    if (dead) return;
    const left = Math.max(0, expiresAt - Date.now());
    const s = Math.ceil(left / 1000);
    el.timer.textContent = String(Math.floor(s / 60)).padStart(2, "0") + ":" + String(s % 60).padStart(2, "0");
    el.timer.classList.toggle("low", s <= 60);
    if (left <= 0) { burn(); return; }
  }
  setInterval(tick, 250);

  function fmtTime(iso) {
    try { return new Date(iso).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }); } catch { return ""; }
  }

  function addMessage(m, fresh) {
    if (seen.has(m.id)) return;
    seen.add(m.id);
    const li = document.createElement("li");
    const head = document.createElement("button");
    head.type = "button";
    head.className = "msg-head" + (fresh ? " new" : "");
    head.setAttribute("aria-expanded", "false");
    const from = document.createElement("span"); from.className = "from"; from.textContent = m.from || "(unknown sender)";
    const time = document.createElement("span"); time.className = "time"; time.textContent = fmtTime(m.received);
    const subj = document.createElement("span"); subj.className = "subj"; subj.textContent = m.subject;
    head.append(from, time, subj);

    const body = document.createElement("div");
    body.className = "msg-body";
    body.hidden = true;

    head.addEventListener("click", () => {
      head.classList.remove("new");
      const open = body.hidden;
      body.hidden = !open;
      head.setAttribute("aria-expanded", String(open));
      if (open && !body.firstChild) {
        const frame = document.createElement("iframe");
        // No allow-scripts and no allow-same-origin: the email can't run code or touch this page.
        frame.setAttribute("sandbox", "allow-popups allow-popups-to-escape-sandbox");
        frame.setAttribute("referrerpolicy", "no-referrer");
        frame.title = "Email: " + m.subject;
        frame.src = "/m/" + encodeURIComponent(token) + "/" + encodeURIComponent(m.id);
        body.append(frame);
        if (m.attachments && m.attachments.length) {
          const p = document.createElement("p");
          p.className = "attachments";
          p.textContent = "Blocked attachments: " + m.attachments.join(", ");
          body.append(p);
        }
      }
    });
    li.append(head, body);
    el.list.prepend(li);
  }

  function updateEmpty() { el.empty.hidden = el.list.children.length > 0; }

  el.copy.addEventListener("click", async () => {
    const text = el.address.textContent;
    let ok = false;
    try { await navigator.clipboard.writeText(text); ok = true; } catch {
      // Fallback for older Safari / non-secure contexts.
      const ta = document.createElement("textarea");
      ta.value = text; ta.setAttribute("readonly", ""); ta.style.position = "fixed"; ta.style.opacity = "0";
      document.body.append(ta); ta.select(); ta.setSelectionRange(0, text.length);
      try { ok = document.execCommand("copy"); } catch {}
      ta.remove();
    }
    el.copy.textContent = ok ? "Copied" : "Select & copy";
    setTimeout(() => (el.copy.textContent = "Copy"), 1500);
  });

  el.renew.addEventListener("click", async () => {
    if (!token) return;
    el.renew.disabled = true;
    const { status, body } = await api("POST", "/api/mailbox/renew");
    if (status === 200) { setExpiry(body); tick(); el.renew.disabled = false; } else { burn(); }
  });

  el.fresh.addEventListener("click", async () => {
    el.fresh.disabled = true;
    await createMailbox();
    el.fresh.disabled = false;
  });

  // Mobile browsers suspend background tabs and drop the stream; catch up on return.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") { tick(); refresh(); }
  });
  window.addEventListener("pageshow", (e) => { if (e.persisted) { tick(); refresh(); } });

  (async () => {
    try {
      if (!(await resume())) await createMailbox();
    } catch {
      showError("Couldn't reach the server. Check your connection and reload.");
    }
  })();
})();
