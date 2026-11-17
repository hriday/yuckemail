package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	domain := flag.String("domain", "yuckemail.online", "mail domain addresses are issued on")
	smtpAddr := flag.String("smtp", ":25", "SMTP listen address")
	httpAddr := flag.String("http", ":8080", "HTTP listen address")
	ttl := flag.Duration("ttl", 5*time.Minute, "mailbox lifetime (and renew length)")
	maxBytes := flag.Int64("max-bytes", 10<<20, "max size of one incoming message")
	trustProxy := flag.Bool("trust-proxy", false, "trust X-Forwarded-For (set when behind Caddy/nginx)")
	flag.Parse()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatal(err)
	}
	store := NewStore(*ttl, 50)
	renderer := &Renderer{key: key}
	limiter := newRateLimiter(30, 10*time.Minute)
	app := &App{
		store:      store,
		renderer:   renderer,
		proxy:      &imageProxy{r: renderer, client: newProxyClient(false)},
		domain:     *domain,
		limiter:    limiter,
		trustProxy: *trustProxy,
	}

	go func() {
		for range time.Tick(2 * time.Second) {
			store.Sweep()
		}
	}()
	go func() {
		for range time.Tick(time.Minute) {
			limiter.Prune()
		}
	}()

	smtpSrv := newSMTPServer(store, *domain, *smtpAddr, *maxBytes)
	go func() {
		log.Printf("SMTP listening on %s for @%s", *smtpAddr, *domain)
		if err := smtpSrv.ListenAndServe(); err != nil {
			log.Fatalf("smtp: %v", err)
		}
	}()

	httpSrv := &http.Server{Addr: *httpAddr, Handler: app.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("HTTP listening on %s", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	smtpSrv.Close()
}
