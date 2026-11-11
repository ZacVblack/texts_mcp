package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rs/zerolog"
)

// Android has no /etc/resolv.conf, so Go's built-in DNS resolver would try
// 127.0.0.1:53 and fail. When that file is missing we resolve through public DNS
// servers instead. On normal computers nothing changes.
var bridgeResolver = net.DefaultResolver

var fallbackDNS = []string{"1.1.1.1:53", "8.8.8.8:53", "1.0.0.1:53", "8.8.4.4:53"}

func applyPlatformNetworkFixes() {
	if _, err := os.Stat("/etc/resolv.conf"); err != nil && runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		bridgeResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				var lastErr error
				for _, server := range fallbackDNS {
					conn, err := d.DialContext(ctx, network, server)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		}
		net.DefaultResolver = bridgeResolver
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

func newDialer() *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Resolver: bridgeResolver}
}

func newLogger(debug bool) zerolog.Logger {
	level := zerolog.InfoLevel
	if debug {
		level = zerolog.DebugLevel
	}
	out := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "Jan 2 15:04:05"}
	return zerolog.New(out).Level(level).With().Timestamp().Logger()
}
