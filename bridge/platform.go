package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// DNS. The Android build uses the phone's own resolver (through cgo), which is
// the one Google Messages uses and so works on any network, including Wi-Fi
// that blocks outside DNS servers. Go's built-in resolver can't find the
// phone's DNS servers on Android (there is no /etc/resolv.conf), so there it
// asks public DNS servers instead. Those servers are also the fallback when the
// phone's resolver can't answer.
var fallbackDNS = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53", "1.0.0.1:53", "8.8.4.4:53"}

// fallbackResolver is set on systems without /etc/resolv.conf (Android).
var fallbackResolver *net.Resolver

var fallbackNext atomic.Uint32

// nextFallbackDNS returns the public DNS server for the next query. Each query
// goes to the next server in turn, so one that a network blocks costs a single
// timeout instead of failing every lookup.
func nextFallbackDNS() string {
	i := fallbackNext.Add(1) - 1
	return fallbackDNS[int(i%uint32(len(fallbackDNS)))]
}

func newFallbackResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, nextFallbackDNS())
		},
	}
}

func applyPlatformNetworkFixes() {
	if _, err := os.Stat("/etc/resolv.conf"); err != nil && runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		fallbackResolver = newFallbackResolver()
		if !systemResolver {
			// A build without cgo can't use the phone's resolver at all.
			net.DefaultResolver = fallbackResolver
		}
	}
	// Termux keeps its CA bundle under $PREFIX. Android's system store also
	// works, but pointing Go at Termux's bundle avoids surprises on old phones.
	if os.Getenv("SSL_CERT_FILE") == "" {
		if prefix := os.Getenv("PREFIX"); prefix != "" {
			bundle := filepath.Join(prefix, "etc", "tls", "cert.pem")
			if _, err := os.Stat(bundle); err == nil {
				_ = os.Setenv("SSL_CERT_FILE", bundle)
			}
		}
	}
}

// bridgeDialer dials with the system resolver and, when that lookup fails,
// looks the name up again through public DNS servers.
type bridgeDialer struct {
	d        net.Dialer
	fallback *net.Resolver
}

func newDialer() *bridgeDialer {
	bd := &bridgeDialer{d: net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}}
	if systemResolver {
		bd.fallback = fallbackResolver
	}
	return bd
}

func (b *bridgeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := b.d.DialContext(ctx, network, addr)
	var dnsErr *net.DNSError
	if err == nil || b.fallback == nil || !errors.As(err, &dnsErr) || ctx.Err() != nil {
		return conn, err
	}
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return nil, err
	}
	ips, lookupErr := b.fallback.LookupIPAddr(ctx, host)
	if lookupErr != nil || len(ips) == 0 {
		return nil, err
	}
	last := err
	for _, ip := range ips {
		c, dialErr := b.d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if dialErr == nil {
			return c, nil
		}
		last = dialErr
	}
	return nil, last
}

func newLogger(debug bool) zerolog.Logger {
	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}
	out := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "Jan 2 15:04:05"}
	return zerolog.New(out).Level(level).With().Timestamp().Logger()
}
