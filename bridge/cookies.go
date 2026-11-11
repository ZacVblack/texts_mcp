package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Cookies Google Messages needs for a Google account pairing. The list matches
// the one used by mautrix-gmessages (pkg/connector/login.go).
var requiredCookies = []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "OSID"}
var optionalCookies = []string{"__Secure-1PSIDTS"}

var (
	reCurlCookieFlag   = regexp.MustCompile(`(?s)(?:^|\s)(?:-b|--cookie)\s+(['"])(.*?)(?:'|")(?:\s|$)`)
	reCurlCookieHeader = regexp.MustCompile(`(?is)-H\s+(['"])cookie:\s*(.*?)(?:'|")(?:\s|$)`)
	rePSCookie         = regexp.MustCompile(`System\.Net\.Cookie\("([^"]+)",\s*"([^"]*)"`)
	reHeaderLine       = regexp.MustCompile(`(?im)^\s*cookie:\s*(.+)$`)
)

// parseCookieInput accepts any of:
//   - "Copy as cURL (bash)" or "Copy as cURL (cmd)" from Chrome or Edge DevTools
//   - "Copy as PowerShell" from Chrome or Edge DevTools
//   - a JSON object like {"SID": "...", ...}
//   - a raw cookie header, "SID=...; HSID=..."
func parseCookieInput(input string) (map[string]string, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return nil, errors.New("nothing was pasted")
	}
	cookies := map[string]string{}

	switch {
	case strings.HasPrefix(text, "{"):
		var obj map[string]any
		if err := json.Unmarshal([]byte(text), &obj); err != nil {
			return nil, fmt.Errorf("that looks like JSON but could not be read: %w", err)
		}
		for k, v := range obj {
			if s, ok := v.(string); ok {
				cookies[k] = s
			}
		}
	case rePSCookie.MatchString(text):
		for _, m := range rePSCookie.FindAllStringSubmatch(text, -1) {
			cookies[m[1]] = m[2]
		}
	case strings.Contains(strings.ToLower(text), "curl"):
		norm := unescapeCmd(text)
		// Join bash line continuations.
		norm = strings.ReplaceAll(norm, "\\\r\n", " ")
		norm = strings.ReplaceAll(norm, "\\\n", " ")
		var raw []string
		for _, m := range reCurlCookieFlag.FindAllStringSubmatch(norm, -1) {
			raw = append(raw, m[2])
		}
		for _, m := range reCurlCookieHeader.FindAllStringSubmatch(norm, -1) {
			raw = append(raw, m[2])
		}
		if len(raw) == 0 {
			return nil, errors.New("the cURL command has no cookies in it; copy the request to messages.google.com/web/config, not a different one")
		}
		for _, r := range raw {
			mergeCookieHeader(cookies, r)
		}
	default:
		if m := reHeaderLine.FindStringSubmatch(text); m != nil {
			text = m[1]
		}
		mergeCookieHeader(cookies, text)
	}

	var missing []string
	for _, name := range requiredCookies {
		if strings.TrimSpace(cookies[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("these cookies are missing: %s. Copy the request to messages.google.com/web/config while signed in", strings.Join(missing, ", "))
	}
	out := map[string]string{}
	for _, name := range append(append([]string{}, requiredCookies...), optionalCookies...) {
		if v := strings.TrimSpace(cookies[name]); v != "" {
			out[name] = v
		}
	}
	return out, nil
}

// unescapeCmd undoes the ^ escaping that "Copy as cURL (cmd)" adds on Windows.
func unescapeCmd(s string) string {
	if !strings.Contains(s, "^") {
		return s
	}
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '^' && i+1 < len(runes) {
			next := runes[i+1]
			if next == '\r' || next == '\n' {
				// Line continuation.
				b.WriteRune(' ')
				i++
				if next == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
					i++
				}
				continue
			}
			b.WriteRune(next)
			i++
			continue
		}
		b.WriteRune(runes[i])
	}
	// cmd format writes %% for a literal %.
	return strings.ReplaceAll(b.String(), "%%", "%")
}

func mergeCookieHeader(dst map[string]string, header string) {
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		dst[strings.TrimSpace(part[:eq])] = strings.TrimSpace(part[eq+1:])
	}
}
