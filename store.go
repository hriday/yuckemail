package main

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"time"
)

// Message is a received email, already parsed. Raw bytes are not kept.
type Message struct {
	ID          string            `json:"id"`
	From        string            `json:"from"`
	Subject     string            `json:"subject"`
	Received    time.Time         `json:"received"`
	Text        string            `json:"text"`
	HasHTML     bool              `json:"hasHtml"`
	Attachments []string          `json:"attachments"` // names only; downloads are blocked
	HTML        string            `json:"-"`
	Inline      map[string]Inline `json:"-"` // keyed by Content-ID
}

type Inline struct {
	ContentType string
	Data        []byte
}

type Mailbox struct {
	Address  string
	Token    string
	Expires  time.Time
	Messages []*Message
	subs     map[chan *Message]struct{}
}

var (
	ErrNotFound = errors.New("mailbox not found or expired")
	ErrFull     = errors.New("mailbox full")
)

// Store keeps every live mailbox in memory. Nothing outlives its mailbox.
type Store struct {
	mu          sync.Mutex
	byAddr      map[string]*Mailbox
	byToken     map[string]*Mailbox
	ttl         time.Duration
	maxMessages int
	now         func() time.Time
}

func NewStore(ttl time.Duration, maxMessages int) *Store {
	return &Store{
		byAddr:      map[string]*Mailbox{},
		byToken:     map[string]*Mailbox{},
		ttl:         ttl,
		maxMessages: maxMessages,
		now:         time.Now,
	}
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func randomString(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// Create makes a new mailbox with a random local part (~50 bits, so reuse is
// practically impossible) and a secret token used to read it.
func (s *Store) Create() *Mailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		local := randomString(7)[:10]
		if _, taken := s.byAddr[local]; taken {
			continue
		}
		mb := &Mailbox{
			Address: local,
			Token:   randomString(20),
			Expires: s.now().Add(s.ttl),
			subs:    map[chan *Message]struct{}{},
		}
		s.byAddr[local] = mb
		s.byToken[mb.Token] = mb
		return mb
	}
}

func (s *Store) liveByToken(token string) (*Mailbox, error) {
	mb, ok := s.byToken[token]
	if !ok || !s.now().Before(mb.Expires) {
		return nil, ErrNotFound
	}
	return mb, nil
}

// Renew resets the countdown to the full TTL.
func (s *Store) Renew(token string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, err := s.liveByToken(token)
	if err != nil {
		return time.Time{}, err
	}
	mb.Expires = s.now().Add(s.ttl)
	return mb.Expires, nil
}

// Snapshot returns the mailbox's expiry and a copy of its message list.
func (s *Store) Snapshot(token string) (string, time.Time, []*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, err := s.liveByToken(token)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	return mb.Address, mb.Expires, append([]*Message(nil), mb.Messages...), nil
}

func (s *Store) Message(token, id string) (*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, err := s.liveByToken(token)
	if err != nil {
		return nil, err
	}
	for _, m := range mb.Messages {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, ErrNotFound
}

// IsLive reports whether mail for local should be accepted.
func (s *Store) IsLive(local string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, ok := s.byAddr[strings.ToLower(local)]
	return ok && s.now().Before(mb.Expires)
}

// Deliver appends a message and notifies any open browser streams.
func (s *Store) Deliver(local string, m *Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, ok := s.byAddr[strings.ToLower(local)]
	if !ok || !s.now().Before(mb.Expires) {
		return ErrNotFound
	}
	if len(mb.Messages) >= s.maxMessages {
		return ErrFull
	}
	m.ID = randomString(8)
	mb.Messages = append(mb.Messages, m)
	for ch := range mb.subs {
		select {
		case ch <- m:
		default: // slow client; it will refetch on reconnect
		}
	}
	return nil
}

// Subscribe returns a channel of new messages, closed when the mailbox dies.
func (s *Store) Subscribe(token string) (chan *Message, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, err := s.liveByToken(token)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan *Message, 16)
	mb.subs[ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := mb.subs[ch]; ok {
			delete(mb.subs, ch)
			close(ch)
		}
	}
	return ch, cancel, nil
}

// Sweep deletes expired mailboxes and everything in them.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for addr, mb := range s.byAddr {
		if now.Before(mb.Expires) {
			continue
		}
		for ch := range mb.subs {
			close(ch)
		}
		mb.subs = nil
		mb.Messages = nil
		delete(s.byAddr, addr)
		delete(s.byToken, mb.Token)
		n++
	}
	return n
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byAddr)
}
