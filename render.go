package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/microcosm-cc/bluemonday"
)

// contentCSP applies to every rendered email: no scripts, no frames, no forms,
// and images only from our own origin (the proxy and inline parts).
const contentCSP = "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox"

type Renderer struct {
	key []byte // HMAC key that signs image-proxy URLs
}

func (r *Renderer) policy(token, msgID string) *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("html", "head", "body", "style", "center", "font", "table", "thead", "tbody", "tfoot", "tr", "td", "th", "span", "div")
	p.AllowUnsafe(true) // keep <style> contents; CSP blocks any remote loads they attempt
	p.AllowAttrs("style", "class", "align", "valign", "width", "height", "bgcolor", "color", "border",
		"cellpadding", "cellspacing", "dir", "face", "size").Globally()
	p.AllowAttrs("src", "alt", "width", "height").OnElements("img")
	p.AllowURLSchemes("http", "https", "mailto", "cid")
	p.AllowDataURIImages()
	p.AllowRelativeURLs(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.RewriteSrc(func(u *url.URL) {
		var dst string
		switch strings.ToLower(u.Scheme) {
		case "cid":
			cid := strings.TrimPrefix(u.String(), u.Scheme+":")
			dst = fmt.Sprintf("/m/%s/%s/cid/%s", token, msgID, url.PathEscape(cid))
		case "http", "https":
			dst = r.ProxyURL(u.String())
		case "data":
			return
		default:
			dst = "/blocked"
		}
		nu, _ := url.Parse(dst)
		*u = *nu
	})
	return p
}

// Sanitize turns untrusted email HTML into something safe to serve under contentCSP.
func (r *Renderer) Sanitize(html, token, msgID string) string {
	return r.policy(token, msgID).Sanitize(html)
}

func (r *Renderer) sign(raw string) string {
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func (r *Renderer) ProxyURL(raw string) string {
	return "/img?u=" + url.QueryEscape(raw) + "&s=" + r.sign(raw)
}

func (r *Renderer) Verify(raw, sig string) bool {
	return hmac.Equal([]byte(r.sign(raw)), []byte(sig))
}

// --- image proxy -------------------------------------------------------

var errForbiddenIP = errors.New("destination not allowed")

// publicOnly is a dialer hook that refuses anything that isn't a public
// unicast address, so the proxy can't be pointed at the VPS or its network.
// It runs after DNS resolution, which also defeats DNS-rebinding tricks.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !isPublicIP(ip) {
		return errForbiddenIP
	}
	return nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPublicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !cgnat.Contains(ip)
}

func newProxyClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		d.Control = publicOnly
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			MaxIdleConnsPerHost:   4,
			ResponseHeaderTimeout: 8 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("bad redirect scheme")
			}
			return nil
		},
	}
}

var safeImageType = regexp.MustCompile(`^image/(png|jpeg|gif|webp|avif|bmp|x-icon|vnd\.microsoft\.icon)$`)

const maxImageBytes = 5 << 20

type imageProxy struct {
	r      *Renderer
	client *http.Client
}

func (p *imageProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, sig := req.URL.Query().Get("u"), req.URL.Query().Get("s")
	if !p.r.Verify(raw, sig) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	out, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	// Reveal nothing about the visitor: generic UA, no cookies, no referer.
	out.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ImageProxy/1.0)")
	out.Header.Set("Accept", "image/*")
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if resp.StatusCode != http.StatusOK || !safeImageType.MatchString(ct) {
		http.Error(w, "not an image", http.StatusUnsupportedMediaType)
		return
	}
	if resp.ContentLength > maxImageBytes {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil || len(body) > maxImageBytes {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	// SVG is deliberately not allowed: it can carry script.
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(body)
}
