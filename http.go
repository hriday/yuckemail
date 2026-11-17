package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

type App struct {
	store      *Store
	renderer   *Renderer
	proxy      http.Handler
	domain     string
	limiter    *rateLimiter
	trustProxy bool
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /", withAppHeaders(files))
	mux.HandleFunc("POST /api/mailbox", a.create)
	mux.HandleFunc("GET /api/mailbox", a.snapshot)
	mux.HandleFunc("POST /api/mailbox/renew", a.renew)
	mux.HandleFunc("GET /api/events", a.events)
	mux.HandleFunc("GET /m/{token}/{id}", a.renderMessage)
	mux.HandleFunc("GET /m/{token}/{id}/cid/{cid}", a.inline)
	mux.Handle("GET /img", a.proxy)
	mux.HandleFunc("GET /blocked", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, "ok %d\n", a.store.Len()) })
	return mux
}

func withAppHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; frame-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

type mailboxJSON struct {
	Address   string     `json:"address"`
	Token     string     `json:"token,omitempty"`
	ExpiresAt time.Time  `json:"expiresAt"`
	ServerNow time.Time  `json:"serverNow"`
	Messages  []*Message `json:"messages"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *App) clientIP(r *http.Request) string {
	if a.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (a *App) create(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow(a.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many new addresses. Try again in a few minutes."})
		return
	}
	mb := a.store.Create()
	writeJSON(w, http.StatusCreated, mailboxJSON{
		Address: mb.Address + "@" + a.domain, Token: mb.Token,
		ExpiresAt: mb.Expires, ServerNow: time.Now(), Messages: []*Message{},
	})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (a *App) snapshot(w http.ResponseWriter, r *http.Request) {
	addr, exp, msgs, err := a.store.Snapshot(bearer(r))
	if err != nil {
		writeJSON(w, http.StatusGone, map[string]string{"error": err.Error()})
		return
	}
	if msgs == nil {
		msgs = []*Message{}
	}
	writeJSON(w, http.StatusOK, mailboxJSON{Address: addr + "@" + a.domain, ExpiresAt: exp, ServerNow: time.Now(), Messages: msgs})
}

func (a *App) renew(w http.ResponseWriter, r *http.Request) {
	exp, err := a.store.Renew(bearer(r))
	if err != nil {
		writeJSON(w, http.StatusGone, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]time.Time{"expiresAt": exp, "serverNow": time.Now()})
}

// events streams new messages with Server-Sent Events. EventSource cannot set
// headers, so the token travels in the query string (HTTPS only in production).
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	ch, cancel, err := a.store.Subscribe(r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusGone)
		return
	}
	defer cancel()
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	rc.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case m, ok := <-ch:
			if !ok {
				fmt.Fprint(w, "event: expired\ndata: {}\n\n")
				rc.Flush()
				return
			}
			b, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: mail\ndata: %s\n\n", b)
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func (a *App) renderMessage(w http.ResponseWriter, r *http.Request) {
	token, id := r.PathValue("token"), r.PathValue("id")
	m, err := a.store.Message(token, id)
	if err != nil {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	var body string
	if m.HasHTML {
		body = a.renderer.Sanitize(m.HTML, token, id)
	} else {
		body = `<pre style="white-space:pre-wrap;word-wrap:break-word;font:14px/1.5 ui-monospace,Menlo,monospace">` +
			html.EscapeString(m.Text) + `</pre>`
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", contentCSPFor(r))
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">`+
		`<meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<style>body{margin:12px;font-family:-apple-system,system-ui,sans-serif;background:#fff;color:#111}img{max-width:100%;height:auto}</style>`)
	fmt.Fprint(w, body)
}

// contentCSPFor lists our origin explicitly as well as 'self', because the
// email document runs with an opaque (sandboxed) origin.
func contentCSPFor(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host
	return strings.Replace(contentCSP, "img-src 'self' data:", "img-src 'self' "+origin+" data:", 1)
}

var inlineImageType = regexp.MustCompile(`^image/(png|jpeg|gif|webp|avif|bmp)$`)

func (a *App) inline(w http.ResponseWriter, r *http.Request) {
	m, err := a.store.Message(r.PathValue("token"), r.PathValue("id"))
	if err != nil {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	part, ok := m.Inline[r.PathValue("cid")]
	if !ok || !inlineImageType.MatchString(strings.ToLower(part.ContentType)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", part.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(part.Data)
}

// --- rate limiting -------------------------------------------------------

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
	return rl
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if now.Sub(t) < rl.window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

func (rl *rateLimiter) Prune() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, ts := range rl.hits {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= rl.window {
			delete(rl.hits, k)
		}
	}
}
