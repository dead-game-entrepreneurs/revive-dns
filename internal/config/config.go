// Package config loads the redirect rules.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole file. ${VAR} references in values are expanded from the
// environment, so IPs can come from .env.
type Config struct {
	// Listen addresses (host:port), served on both UDP and TCP.
	Listen []string `yaml:"listen"`
	// Allow lists client networks; queries from elsewhere are refused. Leave empty
	// only when upstream is empty (redirect-only), otherwise this is an open resolver.
	Allow []string `yaml:"allow"`
	// Upstream resolves every name no rule matches. Empty refuses those names.
	Upstream string `yaml:"upstream"`
	// TTL for answers this server gives itself, in seconds.
	TTL   uint32 `yaml:"ttl"`
	Rules []Rule `yaml:"rules"`

	allow []netip.Prefix
}

// Rule maps names to an answer or to another resolver. A name is exact
// ("gsconnect.ubisoft.com") or a wildcard ("*.dnas.playstation.org", which also
// matches the bare domain).
type Rule struct {
	Service string   `yaml:"service"` // label for logs
	Names   []string `yaml:"names"`
	Answer  string   `yaml:"answer"`  // IPv4 address to answer A queries with
	Forward string   `yaml:"forward"` // resolver host[:port] to pass the query to

	answer netip.Addr
}

// AnswerAddr is the parsed Answer (invalid when the rule forwards).
func (r *Rule) AnswerAddr() netip.Addr { return r.answer }

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse validates a config document.
func Parse(raw []byte) (*Config, error) {
	c := &Config{TTL: 60}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var missing []string
	expand := func(v string) string {
		return os.Expand(v, func(k string) string {
			val, ok := os.LookupEnv(k)
			if !ok {
				missing = append(missing, k)
			}
			return val
		})
	}
	c.Upstream = expand(c.Upstream)
	expandAll(c.Listen, expand)
	expandAll(c.Allow, expand)
	for i := range c.Rules {
		r := &c.Rules[i]
		r.Answer, r.Forward = expand(r.Answer), expand(r.Forward)
		expandAll(r.Names, expand)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: environment variables not set: %s", strings.Join(missing, ", "))
	}
	return c, c.validate()
}

func expandAll(vs []string, expand func(string) string) {
	for i, v := range vs {
		vs[i] = expand(v)
	}
}

func (c *Config) validate() error {
	if len(c.Listen) == 0 {
		return fmt.Errorf("config: listen is empty")
	}
	for _, l := range c.Listen {
		if _, _, err := net.SplitHostPort(l); err != nil {
			return fmt.Errorf("config: listen %q: %w", l, err)
		}
	}
	for _, a := range c.Allow {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			return fmt.Errorf("config: allow %q: %w", a, err)
		}
		c.allow = append(c.allow, p.Masked())
	}
	if c.Upstream != "" && len(c.allow) == 0 {
		return fmt.Errorf("config: an upstream without an allow list is an open resolver")
	}
	if c.Upstream != "" {
		c.Upstream = withPort(c.Upstream)
	}
	seen := map[string]string{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if (r.Answer == "") == (r.Forward == "") {
			return fmt.Errorf("config: rule %q needs exactly one of answer or forward", r.Service)
		}
		if r.Answer != "" {
			a, err := netip.ParseAddr(r.Answer)
			if err != nil || !a.Is4() {
				return fmt.Errorf("config: rule %q: answer %q is not an IPv4 address", r.Service, r.Answer)
			}
			r.answer = a
		} else {
			r.Forward = withPort(r.Forward)
		}
		if len(r.Names) == 0 {
			return fmt.Errorf("config: rule %q has no names", r.Service)
		}
		for j, n := range r.Names {
			n = normalize(n)
			if strings.Count(n, "*") > 1 || (strings.Contains(n, "*") && !strings.HasPrefix(n, "*.")) {
				return fmt.Errorf("config: rule %q: wildcard %q must look like *.example.com", r.Service, n)
			}
			if prev, dup := seen[n]; dup {
				return fmt.Errorf("config: %q is in both %q and %q", n, prev, r.Service)
			}
			seen[n] = r.Service
			r.Names[j] = n
		}
	}
	return nil
}

func withPort(hostport string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(hostport, "53")
}

func normalize(name string) string { return strings.TrimSuffix(strings.ToLower(name), ".") }

// Allowed reports whether a client may query this server.
func (c *Config) Allowed(ip netip.Addr) bool {
	if len(c.allow) == 0 {
		return true
	}
	ip = ip.Unmap()
	for _, p := range c.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Matcher finds the rule for a name: exact names first, then the longest wildcard.
type Matcher struct {
	exact    map[string]*Rule
	wildcard map[string]*Rule // domain without "*." -> rule
}

func (c *Config) Matcher() *Matcher {
	m := &Matcher{exact: map[string]*Rule{}, wildcard: map[string]*Rule{}}
	for i := range c.Rules {
		r := &c.Rules[i]
		for _, n := range r.Names {
			if d, ok := strings.CutPrefix(n, "*."); ok {
				m.wildcard[d] = r
			} else {
				m.exact[n] = r
			}
		}
	}
	return m
}

func (m *Matcher) Match(name string) *Rule {
	name = normalize(name)
	if r, ok := m.exact[name]; ok {
		return r
	}
	for d := name; d != ""; {
		if r, ok := m.wildcard[d]; ok {
			return r
		}
		_, rest, found := strings.Cut(d, ".")
		if !found {
			break
		}
		d = rest
	}
	return nil
}
