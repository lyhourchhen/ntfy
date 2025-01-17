package server

import (
	"bytes"
	"errors"
	"github.com/emersion/go-smtp"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"sync"
)

var (
	errInvalidDomain          = errors.New("invalid domain")
	errInvalidAddress         = errors.New("invalid address")
	errInvalidTopic           = errors.New("invalid topic")
	errTooManyRecipients      = errors.New("too many recipients")
	errUnsupportedContentType = errors.New("unsupported content type")
)

// smtpBackend implements SMTP server methods.
type smtpBackend struct {
	config  *Config
	sub     subscriber
	success int64
	failure int64
	mu      sync.Mutex
}

func newMailBackend(conf *Config, sub subscriber) *smtpBackend {
	return &smtpBackend{
		config: conf,
		sub:    sub,
	}
}

func (b *smtpBackend) Login(state *smtp.ConnectionState, username, password string) (smtp.Session, error) {
	return &smtpSession{backend: b}, nil
}

func (b *smtpBackend) AnonymousLogin(state *smtp.ConnectionState) (smtp.Session, error) {
	return &smtpSession{backend: b}, nil
}

func (b *smtpBackend) Counts() (success int64, failure int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.success, b.failure
}

// smtpSession is returned after EHLO.
type smtpSession struct {
	backend *smtpBackend
	topic   string
	mu      sync.Mutex
}

func (s *smtpSession) AuthPlain(username, password string) error {
	return nil
}

func (s *smtpSession) Mail(from string, opts smtp.MailOptions) error {
	return nil
}

func (s *smtpSession) Rcpt(to string) error {
	return s.withFailCount(func() error {
		conf := s.backend.config
		addressList, err := mail.ParseAddressList(to)
		if err != nil {
			return err
		} else if len(addressList) != 1 {
			return errTooManyRecipients
		}
		to = addressList[0].Address
		if !strings.HasSuffix(to, "@"+conf.SMTPServerDomain) {
			return errInvalidDomain
		}
		to = strings.TrimSuffix(to, "@"+conf.SMTPServerDomain)
		if conf.SMTPServerAddrPrefix != "" {
			if !strings.HasPrefix(to, conf.SMTPServerAddrPrefix) {
				return errInvalidAddress
			}
			to = strings.TrimPrefix(to, conf.SMTPServerAddrPrefix)
		}
		if !topicRegex.MatchString(to) {
			return errInvalidTopic
		}
		s.mu.Lock()
		s.topic = to
		s.mu.Unlock()
		return nil
	})
}

func (s *smtpSession) Data(r io.Reader) error {
	return s.withFailCount(func() error {
		conf := s.backend.config
		b, err := io.ReadAll(r) // Protected by MaxMessageBytes
		if err != nil {
			return err
		}
		msg, err := mail.ReadMessage(bytes.NewReader(b))
		if err != nil {
			return err
		}
		body, err := readMailBody(msg)
		if err != nil {
			return err
		}
		body = strings.TrimSpace(body)
		if len(body) > conf.MessageLimit {
			body = body[:conf.MessageLimit]
		}
		m := newDefaultMessage(s.topic, body)
		subject := strings.TrimSpace(msg.Header.Get("Subject"))
		if subject != "" {
			dec := mime.WordDecoder{}
			subject, err := dec.DecodeHeader(subject)
			if err != nil {
				return err
			}
			m.Title = subject
		}
		if m.Title != "" && m.Message == "" {
			m.Message = m.Title // Flip them, this makes more sense
			m.Title = ""
		}
		if err := s.backend.sub(m); err != nil {
			return err
		}
		s.backend.mu.Lock()
		s.backend.success++
		s.backend.mu.Unlock()
		return nil
	})
}

func (s *smtpSession) Reset() {
	s.mu.Lock()
	s.topic = ""
	s.mu.Unlock()
}

func (s *smtpSession) Logout() error {
	return nil
}

func (s *smtpSession) withFailCount(fn func() error) error {
	err := fn()
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if err != nil {
		s.backend.failure++
	}
	return err
}

func readMailBody(msg *mail.Message) (string, error) {
	contentType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	if contentType == "text/plain" {
		body, err := io.ReadAll(msg.Body)
		if err != nil {
			return "", err
		}
		return string(body), nil
	}
	if strings.HasPrefix(contentType, "multipart/") {
		mr := multipart.NewReader(msg.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil { // may be io.EOF
				return "", err
			}
			partContentType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if err != nil {
				return "", err
			}
			if partContentType != "text/plain" {
				continue
			}
			body, err := io.ReadAll(part)
			if err != nil {
				return "", err
			}
			return string(body), nil
		}
	}
	return "", errUnsupportedContentType
}
