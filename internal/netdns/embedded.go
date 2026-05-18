package netdns

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

// Embedded serves minimal authoritative A records on the bridge gateway UDP/53.
// It is not a recursive resolver; unknown names return NXDOMAIN.
type Embedded struct {
	log *slog.Logger
	gw  net.IP

	mu        sync.RWMutex
	records   map[string]net.IP // FQDN lower case with trailing dot
	server    *dns.Server
	startErr  error
	startOnce sync.Once
}

// NewEmbedded constructs an embedded resolver bound to gw:53 (UDP).
func NewEmbedded(log *slog.Logger, gw net.IP) *Embedded {
	if log == nil {
		log = slog.Default()
	}
	return &Embedded{
		log:     log,
		gw:      gw.To4(),
		records: make(map[string]net.IP),
	}
}

// Register publishes A records for host (short name) and host.nyxd.local.
func (e *Embedded) Register(host, ipStr string) error {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	e.mu.Lock()
	ip4 := ip.To4()
	if ip4 == nil {
		e.mu.Unlock()
		return fmt.Errorf("embedded dns: invalid ipv4 %q", ipStr)
	}
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		e.mu.Unlock()
		return fmt.Errorf("embedded dns: empty host")
	}
	if !isDNSLabel(host) {
		e.mu.Unlock()
		return fmt.Errorf("embedded dns: invalid host label %q", host)
	}
	e.records[dns.Fqdn(host)] = ip4
	e.records[dns.Fqdn(host+".nyxd.local")] = ip4
	e.mu.Unlock()
	e.startOnce.Do(func() { e.startLocked() })
	e.mu.Lock()
	err := e.startErr
	e.mu.Unlock()
	return err
}

func (e *Embedded) startLocked() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.server != nil {
		return
	}
	if e.gw == nil {
		e.startErr = fmt.Errorf("embedded dns: nil gateway")
		return
	}
	addr := net.JoinHostPort(e.gw.String(), "53")
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		e.startErr = fmt.Errorf("embedded dns listen %s: %w", addr, err)
		return
	}
	srv := &dns.Server{
		PacketConn: pc,
		Handler:    dns.HandlerFunc(e.serve),
	}
	e.server = srv
	e.startErr = nil
	go func() {
		if err := srv.ActivateAndServe(); err != nil {
			e.log.Error("embedded dns server exited", "addr", addr, "err", err)
		}
	}()
	e.log.Info("embedded dns listening", "addr", addr)
}

func (e *Embedded) serve(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	if q.Qtype != dns.TypeA {
		m.Rcode = dns.RcodeNotImplemented
		_ = w.WriteMsg(m)
		return
	}
	qname := strings.ToLower(q.Name)
	e.mu.RLock()
	ip, ok := e.records[qname]
	e.mu.RUnlock()
	if !ok {
		m.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(m)
		return
	}
	m.Answer = append(m.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
		A:   ip,
	})
	_ = w.WriteMsg(m)
}

// Deregister removes all names derived from host (short + nyxd.local FQDN).
func (e *Embedded) Deregister(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return nil
	}
	e.mu.Lock()
	delete(e.records, dns.Fqdn(host))
	delete(e.records, dns.Fqdn(host+".nyxd.local"))
	e.mu.Unlock()
	return nil
}

// Shutdown stops the UDP listener.
func (e *Embedded) Shutdown() {
	e.mu.Lock()
	srv := e.server
	e.server = nil
	e.mu.Unlock()
	if srv == nil {
		return
	}
	if err := srv.Shutdown(); err != nil {
		e.log.Warn("embedded dns shutdown", "err", err)
	}
}

func isDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	return true
}

var _ Backend = (*Embedded)(nil)
