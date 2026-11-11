package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers every A query with 127.0.0.1 and refuses AAAA.
func fakeDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true})
			b.EnableCompression()
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60},
					dnsmessage.AResource{A: netip.MustParseAddr("127.0.0.1").As4()})
			}
			out, err := b.Finish()
			if err == nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestFallbackDNSRotates(t *testing.T) {
	seen := map[string]bool{}
	for range fallbackDNS {
		seen[nextFallbackDNS()] = true
	}
	if len(seen) != len(fallbackDNS) {
		t.Fatalf("queries did not rotate through every server: %v", seen)
	}
}

func TestDialerFallsBackToPublicDNS(t *testing.T) {
	old := fallbackDNS
	fallbackDNS = []string{fakeDNS(t)}
	t.Cleanup(func() { fallbackDNS = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	// .invalid never resolves through the system, like a lookup a network blocks.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bd := &bridgeDialer{d: net.Dialer{Timeout: 5 * time.Second}, fallback: newFallbackResolver()}
	conn, err := bd.DialContext(ctx, "tcp", "blocked-by-network.invalid:"+port)
	if err != nil {
		t.Fatalf("fallback lookup did not connect: %v", err)
	}
	conn.Close()

	// Without a fallback the DNS error comes back unchanged.
	plain := &bridgeDialer{d: net.Dialer{Timeout: 5 * time.Second}}
	_, err = plain.DialContext(ctx, "tcp", "blocked-by-network.invalid:"+port)
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("want a DNS error, got %v", err)
	}
}

func TestDialerKeepsNonDNSErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	bd := &bridgeDialer{d: net.Dialer{Timeout: 2 * time.Second}, fallback: newFallbackResolver()}
	_, err = bd.DialContext(context.Background(), "tcp", addr)
	var dnsErr *net.DNSError
	if err == nil || errors.As(err, &dnsErr) {
		t.Fatalf("want a connection error, got %v", err)
	}
}
