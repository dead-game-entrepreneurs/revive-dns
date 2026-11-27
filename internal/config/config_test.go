package config

import (
	"net/netip"
	"strings"
	"testing"
)

const sample = `
# ${VARS} in comments are left alone
listen: ["127.0.0.1:5353"]
allow: ["192.0.2.0/24"]
upstream: "1.1.1.1"
rules:
  - service: dnas
    names: ["*.dnas.playstation.org"]
    forward: "192.0.2.54"
  - service: game
    names: ["Game.Example.com.", "*.lobby.example.com"]
    answer: "${GAME_IP}"
`

func TestParseAndMatch(t *testing.T) {
	t.Setenv("GAME_IP", "192.0.2.10")
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream != "1.1.1.1:53" || c.Rules[0].Forward != "192.0.2.54:53" {
		t.Fatalf("ports not defaulted: %q %q", c.Upstream, c.Rules[0].Forward)
	}
	m := c.Matcher()
	for name, want := range map[string]string{
		"game.example.com":              "game",
		"GAME.EXAMPLE.COM.":             "game",
		"eu.lobby.example.com":          "game",
		"lobby.example.com":             "game", // a wildcard also covers the bare domain
		"gate1.us.dnas.playstation.org": "dnas",
		"example.com":                   "",
		"notgame.example.com":           "",
	} {
		got := ""
		if r := m.Match(name); r != nil {
			got = r.Service
		}
		if got != want {
			t.Errorf("Match(%q) = %q, want %q", name, got, want)
		}
	}
	if !c.Allowed(netip.MustParseAddr("192.0.2.50")) || c.Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("allow list not applied")
	}
}

func TestRejects(t *testing.T) {
	t.Setenv("GAME_IP", "192.0.2.10")
	for name, doc := range map[string]string{
		"missing env":   strings.Replace(sample, "${GAME_IP}", "${NOPE_NOT_SET}", 1),
		"open resolver": strings.Replace(sample, `allow: ["192.0.2.0/24"]`, "", 1),
		"bad answer":    strings.Replace(sample, "${GAME_IP}", "not-an-ip", 1),
		"duplicate":     strings.Replace(sample, `"*.lobby.example.com"`, `"*.dnas.playstation.org"`, 1),
		"bad wildcard":  strings.Replace(sample, `"*.lobby.example.com"`, `"lobby.*.com"`, 1),
		"unknown key":   sample + "\nbogus: 1\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
