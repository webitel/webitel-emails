// Package mailtest runs a scripted SMTP server for tests.
package mailtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Replies with special behavior.
const (
	// FinalDrop closes the connection without a reply; also for "DATA".
	FinalDrop = "drop"
	// FinalHang never replies; the client has to give up.
	FinalHang = "hang"
)

// Config scripts SMTP replies by command, with success by default.
// Use EHLO#1 for the first EHLO and RCPT <address> for a specific recipient.
type Config struct {
	// StartTLS offers STARTTLS on a plain listener instead of implicit TLS.
	StartTLS bool
	// Size is the advertised SIZE; 0 advertises none.
	Size int64
	// Mechanisms advertised for AUTH; empty offers PLAIN, LOGIN and XOAUTH2.
	Mechanisms string
	Replies    map[string]string
	DropOnQuit bool
	// Delay holds the reply to the final dot.
	Delay time.Duration
}

// Message is what one session sent.
type Message struct {
	Mechanism  string
	From       string
	Recipients []string
	Data       []byte
}

// Server is a running fake SMTP server.
type Server struct {
	Host    string
	Port    int32
	RootCAs *x509.CertPool

	cfg      Config
	tls      *tls.Config
	listener net.Listener

	mu        sync.Mutex
	accepted  []Message
	sessions  int
	discarded int
	active    int
	maxActive int
}

// Start listens on 127.0.0.1 until the test ends.
func Start(t testing.TB, cfg Config) *Server {
	t.Helper()
	cert, pool := certificate(t)
	s := &Server{Host: "127.0.0.1", RootCAs: pool, cfg: cfg, tls: &tls.Config{Certificates: []tls.Certificate{cert}}}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StartTLS {
		listener = tls.NewListener(listener, s.tls)
	}
	s.listener = listener
	s.Port = int32(listener.Addr().(*net.TCPAddr).Port)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()

	return s
}

// Accepted returns the emails the server confirmed after the final dot.
func (s *Server) Accepted() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Message(nil), s.accepted...)
}

// MaxActive is the largest number of sessions open at the same time.
func (s *Server) MaxActive() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.maxActive
}

// Sessions counts the connections; Discarded counts bodies cut before the final dot.
func (s *Server) Sessions() (sessions, discarded int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sessions, s.discarded
}

func (s *Server) reply(key string) string {
	return s.cfg.Replies[key]
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	s.mu.Lock()
	s.sessions++
	s.active++
	s.maxActive = max(s.maxActive, s.active)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	text := textproto.NewConn(conn)
	secure := !s.cfg.StartTLS
	var m Message
	ehlos := 0
	if reply := s.reply("GREETING"); reply != "" {
		_ = text.PrintfLine("%s", reply)
		return
	}
	_ = text.PrintfLine("220 fake ESMTP")

	for {
		line, err := text.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if reply := s.reply("HELO"); reply != "" && strings.EqualFold(verb, "HELO") {
				_ = text.PrintfLine("%s", reply)
				continue
			}
			if strings.EqualFold(verb, "EHLO") {
				ehlos++
				if reply := s.reply("EHLO#1"); reply != "" && ehlos == 1 {
					_ = text.PrintfLine("%s", reply)
					continue
				}
			}
			if reply := s.reply("EHLO"); reply != "" {
				_ = text.PrintfLine("%s", reply)
				continue
			}
			lines := []string{"fake"}
			if s.cfg.Size > 0 {
				lines = append(lines, "SIZE "+strconv.FormatInt(s.cfg.Size, 10))
			}
			if !secure {
				lines = append(lines, "STARTTLS")
			} else {
				mechanisms := s.cfg.Mechanisms
				if mechanisms == "" {
					mechanisms = "PLAIN LOGIN XOAUTH2"
				}
				lines = append(lines, "AUTH "+mechanisms)
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = text.PrintfLine("250%s%s", sep, l)
			}
		case "STARTTLS":
			if reply := s.reply("STARTTLS"); reply != "" {
				_ = text.PrintfLine("%s", reply)
				continue
			}
			_ = text.PrintfLine("220 ready")
			tlsConn := tls.Server(conn, s.tls)
			if tlsConn.Handshake() != nil {
				return
			}
			conn, secure = tlsConn, true
			text = textproto.NewConn(conn)
		case "AUTH":
			mechanism, initial, _ := strings.Cut(arg, " ")
			m.Mechanism = strings.ToUpper(mechanism)
			if m.Mechanism == "LOGIN" {
				for _, prompt := range []string{"VXNlcm5hbWU6", "UGFzc3dvcmQ6"} {
					_ = text.PrintfLine("334 %s", prompt)
					if _, err := text.ReadLine(); err != nil {
						return
					}
				}
			} else if _, err := base64.StdEncoding.DecodeString(initial); err != nil {
				_ = text.PrintfLine("501 bad response")
				continue
			}
			s.answer(text, "AUTH", "235 ok")
		case "MAIL":
			m.From = address(arg)
			s.answer(text, "MAIL", "250 ok")
		case "RCPT":
			rcpt := address(arg)
			if reply := s.reply("RCPT " + rcpt); reply != "" {
				_ = text.PrintfLine("%s", reply)
				continue
			}
			if s.answer(text, "RCPT", "250 ok") {
				m.Recipients = append(m.Recipients, rcpt)
			}
		case "DATA":
			if s.reply("DATA") == FinalDrop {
				return
			}
			if !s.answer(text, "DATA", "354 go on") {
				continue
			}
			data, err := io.ReadAll(text.DotReader())
			if err != nil {
				s.mu.Lock()
				s.discarded++
				s.mu.Unlock()
				return
			}
			time.Sleep(s.cfg.Delay)
			switch final := s.reply("FINAL"); final {
			case FinalDrop:
				s.accept(m, data)
				return
			case FinalHang:
				s.accept(m, data)
				_, _ = io.Copy(io.Discard, conn)
				return
			default:
				if final == "" || strings.HasPrefix(final, "2") {
					s.accept(m, data)
				}
				if final == "" {
					final = "250 queued"
				}
				_ = text.PrintfLine("%s", final)
			}
		case "RSET":
			_ = text.PrintfLine("250 ok")
		case "QUIT":
			if !s.cfg.DropOnQuit {
				_ = text.PrintfLine("221 bye")
			}
			return
		default:
			_ = text.PrintfLine("502 unknown")
		}
	}
}

// answer writes the scripted reply or fallback and tells whether it succeeded.
func (s *Server) answer(text *textproto.Conn, key, fallback string) bool {
	reply := s.reply(key)
	if reply == "" {
		reply = fallback
	}
	_ = text.PrintfLine("%s", reply)

	return reply[0] == '2' || reply[0] == '3'
}

func (s *Server) accept(m Message, data []byte) {
	m.Data = data
	s.mu.Lock()
	s.accepted = append(s.accepted, m)
	s.mu.Unlock()
}

func address(arg string) string {
	start, end := strings.IndexByte(arg, '<'), strings.IndexByte(arg, '>')
	if start < 0 || end < start {
		return ""
	}

	return arg[start+1 : end]
}

func certificate(t testing.TB) (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}
