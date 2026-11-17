package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := NewStore(5*time.Minute, 2)
	s.now = func() time.Time { return now }

	mb := s.Create()
	if len(mb.Address) != 10 || !s.IsLive(mb.Address) {
		t.Fatalf("new mailbox should be live: %+v", mb)
	}
	if !s.IsLive(strings.ToUpper(mb.Address)) {
		t.Fatal("address match should be case-insensitive")
	}

	now = now.Add(4 * time.Minute)
	if _, err := s.Renew(mb.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute) // 8m after creation, 4m after renew
	if !s.IsLive(mb.Address) {
		t.Fatal("renew should have extended the lifetime")
	}

	for i := 0; i < 2; i++ {
		if err := s.Deliver(mb.Address, &Message{Subject: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Deliver(mb.Address, &Message{}); err != ErrFull {
		t.Fatalf("want ErrFull, got %v", err)
	}

	now = now.Add(2 * time.Minute)
	if s.IsLive(mb.Address) {
		t.Fatal("should be expired")
	}
	if _, err := s.Renew(mb.Token); err != ErrNotFound {
		t.Fatal("expired mailbox must not be renewable")
	}
	if n := s.Sweep(); n != 1 || s.Len() != 0 {
		t.Fatalf("sweep removed %d, %d left", n, s.Len())
	}
}

func TestSubscribeClosesOnSweep(t *testing.T) {
	now := time.Now()
	s := NewStore(time.Minute, 10)
	s.now = func() time.Time { return now }
	mb := s.Create()
	ch, cancel, err := s.Subscribe(mb.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	s.Deliver(mb.Address, &Message{Subject: "x"})
	if m := <-ch; m.Subject != "x" {
		t.Fatal("expected message on channel")
	}
	now = now.Add(2 * time.Minute)
	s.Sweep()
	if _, ok := <-ch; ok {
		t.Fatal("channel should close when mailbox dies")
	}
}

func startSMTP(t *testing.T, s *Store) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newSMTPServer(s, "yuckemail.online", "", 1<<20)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

const testMail = "From: Shady <noreply@shady.example>\r\n" +
	"To: %s\r\nSubject: Your code\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
	"<p>Code: <b>123456</b></p><script>alert(1)</script><img src=\"https://t.example/p.gif\">\r\n" +
	"--B\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n\r\nPDFDATA\r\n" +
	"--B--\r\n"

func TestSMTPAcceptsLiveRejectsOthers(t *testing.T) {
	s := NewStore(time.Minute, 10)
	addr := startSMTP(t, s)
	mb := s.Create()
	to := mb.Address + "@yuckemail.online"

	msg := strings.Replace(testMail, "%s", to, 1)
	if err := smtp.SendMail(addr, nil, "noreply@shady.example", []string{to}, []byte(msg)); err != nil {
		t.Fatalf("live address should accept: %v", err)
	}
	_, _, msgs, _ := s.Snapshot(mb.Token)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Subject != "Your code" || !m.HasHTML || len(m.Attachments) != 1 || m.Attachments[0] != "invoice.pdf" {
		t.Fatalf("bad parse: %+v", m)
	}

	err := smtp.SendMail(addr, nil, "a@b.example", []string{"nobody12345@yuckemail.online"}, []byte(msg))
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("unknown address should get 550, got %v", err)
	}
	err = smtp.SendMail(addr, nil, "a@b.example", []string{"victim@gmail.com"}, []byte(msg))
	if err == nil {
		t.Fatal("must not accept mail for other domains")
	}
}

func TestSanitize(t *testing.T) {
	r := &Renderer{key: []byte("k")}
	in := `<html><head><style>p{color:red}</style></head><body>` +
		`<p onclick="evil()" style="color:blue">hi</p><script>alert(1)</script>` +
		`<img src="https://t.example/p.gif"><img src="cid:logo@x"><img src="javascript:alert(1)">` +
		`<a href="https://example.com">link</a><a href="javascript:alert(1)">bad</a>` +
		`<iframe src="https://evil"></iframe><form action="https://evil"><input></form></body></html>`
	out := r.Sanitize(in, "TOK", "MID")
	for _, bad := range []string{"<script", "onclick", "javascript:", "<iframe", "<form", "https://t.example/p.gif\""} {
		if strings.Contains(out, bad) {
			t.Errorf("output still contains %q:\n%s", bad, out)
		}
	}
	for _, good := range []string{"p{color:red}", `style="color:blue"`, "/img?u=https%3A%2F%2Ft.example%2Fp.gif&amp;s=",
		"/m/TOK/MID/cid/logo@x", `target="_blank"`, "noreferrer"} {
		if !strings.Contains(out, good) {
			t.Errorf("output missing %q:\n%s", good, out)
		}
	}
}

func TestProxyRefusesBadRequests(t *testing.T) {
	r := &Renderer{key: []byte("k")}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNG"))
	}))
	defer backend.Close()

	strict := &imageProxy{r: r, client: newProxyClient(false)}
	loose := &imageProxy{r: r, client: newProxyClient(true)}

	get := func(p http.Handler, target, sig string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/img?u="+target+"&s="+sig, nil)
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	u := backend.URL + "/x.png"
	if c := get(strict, u, "forged"); c != http.StatusForbidden {
		t.Errorf("unsigned URL: got %d", c)
	}
	if c := get(strict, u, r.sign(u)); c != http.StatusBadGateway {
		t.Errorf("loopback must be refused (SSRF), got %d", c)
	}
	if c := get(loose, u, r.sign(u)); c != http.StatusOK {
		t.Errorf("public fetch path should work, got %d", c)
	}
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "169.254.169.254", "127.0.0.1", "100.64.0.1", "::1", "fc00::1"} {
		if isPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s treated as public", ip)
		}
	}
}

func TestHTTPFlow(t *testing.T) {
	store := NewStore(time.Minute, 10)
	r := &Renderer{key: []byte("k")}
	app := &App{store: store, renderer: r, proxy: &imageProxy{r: r, client: newProxyClient(false)},
		domain: "yuckemail.online", limiter: newRateLimiter(2, time.Minute)}
	srv := httptest.NewServer(app.Routes())
	defer srv.Close()

	res, _ := http.Post(srv.URL+"/api/mailbox", "", nil)
	var mb mailboxJSON
	json.NewDecoder(res.Body).Decode(&mb)
	if res.StatusCode != 201 || !strings.HasSuffix(mb.Address, "@yuckemail.online") || mb.Token == "" {
		t.Fatalf("create: %d %+v", res.StatusCode, mb)
	}
	local := strings.TrimSuffix(mb.Address, "@yuckemail.online")
	m, _ := parseMessage([]byte(strings.Replace(testMail, "%s", mb.Address, 1)))
	store.Deliver(local, m)

	// Reading requires the token, not the address.
	req, _ := http.NewRequest("GET", srv.URL+"/api/mailbox", nil)
	req.Header.Set("Authorization", "Bearer "+local)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusGone {
		t.Fatalf("address alone must not grant access, got %d", res.StatusCode)
	}

	res, _ = http.Get(srv.URL + "/m/" + mb.Token + "/" + m.ID)
	body, _ := io.ReadAll(res.Body)
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
		t.Fatalf("missing strict CSP: %q", csp)
	}
	if strings.Contains(string(body), "<script") || !strings.Contains(string(body), "123456") {
		t.Fatalf("bad render: %s", body)
	}

	http.Post(srv.URL+"/api/mailbox", "", nil)
	if res, _ := http.Post(srv.URL+"/api/mailbox", "", nil); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limit not enforced: %d", res.StatusCode)
	}
}
