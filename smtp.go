package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/jhillyerd/enmime"
)

type smtpBackend struct {
	store  *Store
	domain string
}

func (b *smtpBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{b: b}, nil
}

type smtpSession struct {
	b     *smtpBackend
	rcpts []string
}

var errNoMailbox = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such mailbox"}

func (s *smtpSession) Mail(string, *smtp.MailOptions) error { return nil }

// Rcpt rejects any address that is not live right now, so burned addresses
// drop mail at the door and nothing is stored for them.
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	addr, err := mail.ParseAddress(to)
	if err != nil {
		return errNoMailbox
	}
	at := strings.LastIndexByte(addr.Address, '@')
	if at < 0 || !strings.EqualFold(addr.Address[at+1:], s.b.domain) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Relaying denied"}
	}
	local := strings.ToLower(addr.Address[:at])
	if !s.b.store.IsLive(local) {
		return errNoMailbox
	}
	s.rcpts = append(s.rcpts, local)
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	delivered := 0
	for _, local := range s.rcpts {
		m, err := parseMessage(raw) // parse per recipient so messages share nothing
		if err != nil {
			return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "Unparseable message"}
		}
		switch err := s.b.store.Deliver(local, m); {
		case err == nil:
			delivered++
		case errors.Is(err, ErrFull):
			log.Printf("mailbox full, dropping")
		}
	}
	if delivered == 0 {
		return errNoMailbox
	}
	return nil
}

func (s *smtpSession) Reset()        { s.rcpts = nil }
func (s *smtpSession) Logout() error { return nil }

func parseMessage(raw []byte) (*Message, error) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	m := &Message{
		From:     env.GetHeader("From"),
		Subject:  env.GetHeader("Subject"),
		Received: time.Now(),
		Text:     env.Text,
		HTML:     env.HTML,
		HasHTML:  strings.TrimSpace(env.HTML) != "",
		Inline:   map[string]Inline{},
	}
	if m.Subject == "" {
		m.Subject = "(no subject)"
	}
	for _, p := range append(append([]*enmime.Part{}, env.Inlines...), env.OtherParts...) {
		if p.ContentID != "" && strings.HasPrefix(p.ContentType, "image/") {
			m.Inline[p.ContentID] = Inline{ContentType: p.ContentType, Data: p.Content}
		}
	}
	for _, p := range env.Attachments {
		name := p.FileName
		if name == "" {
			name = "(unnamed " + p.ContentType + ")"
		}
		m.Attachments = append(m.Attachments, name)
	}
	if m.Attachments == nil {
		m.Attachments = []string{}
	}
	return m, nil
}

func newSMTPServer(store *Store, domain, addr string, maxBytes int64) *smtp.Server {
	srv := smtp.NewServer(&smtpBackend{store: store, domain: domain})
	srv.Addr = addr
	srv.Domain = domain
	srv.MaxMessageBytes = maxBytes
	srv.MaxRecipients = 20
	srv.ReadTimeout = 30 * time.Second
	srv.WriteTimeout = 30 * time.Second
	return srv
}
