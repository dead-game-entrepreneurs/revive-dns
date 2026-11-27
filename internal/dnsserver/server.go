// Package dnsserver answers DNS queries from the redirect rules.
//
// A matching "answer" rule gets an authoritative reply: an A record for A queries
// and an empty NOERROR for any other type (so clients never fall through to the
// real IPv6 address). A matching "forward" rule, and anything unmatched when an
// upstream is configured, is relayed verbatim to that resolver.
package dnsserver

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/dead-game-entrepreneurs/revive-dns/internal/config"
)

const (
	upstreamTimeout = 4 * time.Second
	tcpIdleTimeout  = 10 * time.Second
	maxInFlight     = 512 // concurrent UDP queries
)

type Server struct {
	cfg   *config.Config
	match *config.Matcher
	log   *slog.Logger
	sem   chan struct{}
}

func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, match: cfg.Matcher(), log: log, sem: make(chan struct{}, maxInFlight)}
}

// Run serves every listen address on UDP and TCP until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	errs := make(chan error, 2*len(s.cfg.Listen))
	var wg sync.WaitGroup
	for _, addr := range s.cfg.Listen {
		pc, err := lc.ListenPacket(ctx, "udp", addr)
		if err != nil {
			return err
		}
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			pc.Close()
			return err
		}
		go func() { <-ctx.Done(); pc.Close(); ln.Close() }()
		wg.Add(2)
		go func() { defer wg.Done(); errs <- s.serveUDP(ctx, pc) }()
		go func() { defer wg.Done(); errs <- s.serveTCP(ctx, ln) }()
		s.log.Info("listening", "addr", addr)
	}
	select {
	case <-ctx.Done():
		wg.Wait()
		return ctx.Err()
	case err := <-errs:
		return err
	}
}

func (s *Server) serveUDP(ctx context.Context, pc net.PacketConn) error {
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		query := append([]byte(nil), buf[:n]...)
		s.sem <- struct{}{}
		go func() {
			defer func() { <-s.sem }()
			if reply := s.Handle(ctx, query, addrOf(from), false); reply != nil {
				pc.WriteTo(reply, from)
			}
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context, ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			continue
		}
		go s.tcpConn(ctx, c)
	}
}

func (s *Server) tcpConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	for {
		c.SetDeadline(time.Now().Add(tcpIdleTimeout))
		query, err := readTCP(c)
		if err != nil {
			return
		}
		reply := s.Handle(ctx, query, addrOf(c.RemoteAddr()), true)
		if reply == nil || writeTCP(c, reply) != nil {
			return
		}
	}
}

func addrOf(a net.Addr) netip.Addr {
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// Handle returns the reply for one query, or nil to drop it.
func (s *Server) Handle(ctx context.Context, query []byte, client netip.Addr, tcp bool) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil || hdr.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return reply(hdr, nil, dnsmessage.RCodeFormatError, nil, 0)
	}
	name := q.Name.String()
	if !s.cfg.Allowed(client) {
		s.log.Debug("refused client", "client", client, "name", name)
		return reply(hdr, &q, dnsmessage.RCodeRefused, nil, 0)
	}

	rule := s.match.Match(name)
	switch {
	case rule != nil && rule.Forward == "":
		s.log.Info("redirect", "service", rule.Service, "client", client, "name", name,
			"type", q.Type, "answer", rule.AnswerAddr())
		var a *[4]byte
		if q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET {
			ip := rule.AnswerAddr().As4()
			a = &ip
		}
		return reply(hdr, &q, dnsmessage.RCodeSuccess, a, s.cfg.TTL)
	case rule != nil:
		s.log.Info("forward", "service", rule.Service, "client", client, "name", name, "to", rule.Forward)
		return s.forward(ctx, hdr, &q, query, rule.Forward, tcp)
	case s.cfg.Upstream != "":
		s.log.Debug("upstream", "client", client, "name", name)
		return s.forward(ctx, hdr, &q, query, s.cfg.Upstream, tcp)
	default:
		return reply(hdr, &q, dnsmessage.RCodeRefused, nil, 0)
	}
}

func (s *Server) forward(ctx context.Context, hdr dnsmessage.Header, q *dnsmessage.Question,
	query []byte, to string, tcp bool) []byte {
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	resp, err := exchange(ctx, query, to, tcp)
	if err != nil {
		s.log.Warn("forward failed", "to", to, "name", q.Name.String(), "err", err)
		return reply(hdr, q, dnsmessage.RCodeServerFailure, nil, 0)
	}
	return resp
}

// exchange relays a query over the same transport the client used; a truncated
// UDP answer is passed back as-is so the client retries over TCP.
func exchange(ctx context.Context, query []byte, to string, tcp bool) ([]byte, error) {
	var d net.Dialer
	network := "udp"
	if tcp {
		network = "tcp"
	}
	c, err := d.DialContext(ctx, network, to)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if tcp {
		if err := writeTCP(c, query); err != nil {
			return nil, err
		}
		return readTCP(c)
	}
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 12 && buf[0] == query[0] && buf[1] == query[1] {
			return buf[:n], nil
		}
	}
}

func reply(hdr dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, a *[4]byte, ttl uint32) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: hdr.ID, Response: true, Authoritative: rcode == dnsmessage.RCodeSuccess,
		RecursionDesired: hdr.RecursionDesired, RecursionAvailable: true, RCode: rcode,
	})
	b.EnableCompression()
	if q != nil {
		b.StartQuestions()
		b.Question(*q)
		if a != nil {
			b.StartAnswers()
			b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: ttl},
				dnsmessage.AResource{A: *a})
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

func readTCP(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, msg)
	return msg, err
}

func writeTCP(w io.Writer, msg []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...))
	return err
}
