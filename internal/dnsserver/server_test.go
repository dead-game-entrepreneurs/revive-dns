package dnsserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/dead-game-entrepreneurs/revive-dns/internal/config"
)

func query(t *testing.T, name string, qt dnsmessage.Type) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qt, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parse(t *testing.T, b []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatal(err)
	}
	return m.Header, m.Answers
}

// fakeUpstream answers every A query with 203.0.113.99.
func fakeUpstream(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, _ := p.Start(buf[:n])
			q, _ := p.Question()
			pc.WriteTo(reply(h, &q, dnsmessage.RCodeSuccess, &[4]byte{203, 0, 113, 99}, 30), from)
		}
	}()
	return pc.LocalAddr().String()
}

func newServer(t *testing.T, upstream string) *Server {
	doc := `
listen: ["127.0.0.1:0"]
allow: ["192.0.2.0/24"]
upstream: "` + upstream + `"
rules:
  - service: dnas
    names: ["*.dnas.playstation.org"]
    forward: "` + fakeUpstream(t) + `"
  - service: angelware
    names: ["smackdownvsraw2007.stun.us.demonware.net"]
    answer: "192.0.2.10"
`
	if upstream == "" {
		doc = `
listen: ["127.0.0.1:0"]
rules:
  - service: angelware
    names: ["smackdownvsraw2007.stun.us.demonware.net"]
    answer: "192.0.2.10"
`
	}
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var lan = netip.MustParseAddr("192.0.2.50")

func TestRedirect(t *testing.T) {
	s := newServer(t, fakeUpstream(t))
	h, ans := parse(t, s.Handle(context.Background(),
		query(t, "SmackDownVsRaw2007.stun.us.demonware.net.", dnsmessage.TypeA), lan, false))
	if h.ID != 0x1234 || h.RCode != dnsmessage.RCodeSuccess || len(ans) != 1 ||
		ans[0].Body.(*dnsmessage.AResource).A != [4]byte{192, 0, 2, 10} {
		t.Fatalf("header %+v answers %v", h, ans)
	}
	// Other record types for a redirected name: empty success, no leak to the real host.
	h, ans = parse(t, s.Handle(context.Background(),
		query(t, "smackdownvsraw2007.stun.us.demonware.net.", dnsmessage.TypeAAAA), lan, false))
	if h.RCode != dnsmessage.RCodeSuccess || len(ans) != 0 {
		t.Fatalf("AAAA: %+v %v", h, ans)
	}
}

func TestForwardAndUpstream(t *testing.T) {
	s := newServer(t, fakeUpstream(t))
	for _, name := range []string{"gate1.us.dnas.playstation.org.", "example.com."} {
		h, ans := parse(t, s.Handle(context.Background(), query(t, name, dnsmessage.TypeA), lan, false))
		if h.RCode != dnsmessage.RCodeSuccess || len(ans) != 1 ||
			ans[0].Body.(*dnsmessage.AResource).A != [4]byte{203, 0, 113, 99} {
			t.Fatalf("%s: %+v %v", name, h, ans)
		}
	}
}

func TestRefusals(t *testing.T) {
	s := newServer(t, fakeUpstream(t))
	h, _ := parse(t, s.Handle(context.Background(), query(t, "example.com.", dnsmessage.TypeA),
		netip.MustParseAddr("8.8.8.8"), false))
	if h.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("outside client: %v", h.RCode)
	}
	// Redirect-only mode: unknown names are refused, game names still answered.
	s = newServer(t, "")
	if h, _ := parse(t, s.Handle(context.Background(), query(t, "example.com.", dnsmessage.TypeA),
		netip.MustParseAddr("8.8.8.8"), false)); h.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("redirect-only unknown: %v", h.RCode)
	}
	if _, ans := parse(t, s.Handle(context.Background(),
		query(t, "smackdownvsraw2007.stun.us.demonware.net.", dnsmessage.TypeA),
		netip.MustParseAddr("8.8.8.8"), false)); len(ans) != 1 {
		t.Fatal("redirect-only game name not answered")
	}
}
