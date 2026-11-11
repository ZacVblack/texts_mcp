package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// MinPassphraseLength matches the Worker's own check.
const MinPassphraseLength = 16

// Config is what this device needs to talk to the Worker.
// It holds the passphrase, so the file is written with owner-only permissions.
type Config struct {
	WorkerURL  string `json:"worker_url"`
	Passphrase string `json:"passphrase"`
	// TimeZone is only used for log lines; the Worker formats times for Claude.
	TimeZone string `json:"time_zone,omitempty"`
}

func defaultConfigDir() string {
	if dir := os.Getenv("TEXTS_MCP_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".texts-mcp"
	}
	return filepath.Join(home, ".texts-mcp")
}

func defaultConfigPath() string {
	return filepath.Join(defaultConfigDir(), "config.json")
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s; run `texts-bridge setup` first", path)
	} else if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config file %s is not valid JSON: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	u, err := normalizeWorkerURL(c.WorkerURL)
	if err != nil {
		return err
	}
	c.WorkerURL = u
	if utf8.RuneCountInString(c.Passphrase) < MinPassphraseLength {
		return fmt.Errorf("passphrase must be at least %d characters", MinPassphraseLength)
	}
	return nil
}

// normalizeWorkerURL accepts "texts-mcp.name.workers.dev" or a full https URL
// and returns the origin with no trailing slash.
func normalizeWorkerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("worker address is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("worker address is not a valid URL: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return "", errors.New("worker address must start with https://")
	}
	if u.Host == "" {
		return "", errors.New("worker address has no host name")
	}
	return u.Scheme + "://" + u.Host, nil
}

func saveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cmdSetup(ctx context.Context, path string) error {
	in := bufio.NewReader(os.Stdin)
	fmt.Println("Setting up texts-bridge on this device.")
	fmt.Println()
	fmt.Println("Worker address, for example https://texts-mcp.yourname.workers.dev")
	fmt.Print("> ")
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("reading worker address: %w", err)
	}
	workerURL, err := normalizeWorkerURL(line)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("Passphrase (the same one you saved as the Worker's PASSPHRASE secret, at least %d characters).\n", MinPassphraseLength)
	fmt.Println("Typing is hidden.")
	pass, err := readSecret(in)
	if err != nil {
		return err
	}
	cfg := &Config{WorkerURL: workerURL, Passphrase: pass}
	if err := cfg.validate(); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("Checking the Worker...")
	wc := newWorkerClient(cfg)
	if _, err := wc.Ping(ctx); err != nil {
		return fmt.Errorf("the Worker did not accept this address and passphrase: %w", err)
	}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Println("Saved to", path)
	fmt.Println("The Worker accepted the passphrase.")
	return nil
}

func readSecret(in *bufio.Reader) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Print("> ")
		b, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func cmdURL(path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	fmt.Println(connectorURL(cfg))
	return nil
}

func connectorURL(cfg *Config) string {
	return cfg.WorkerURL + "/mcp/" + deriveToken(cfg.Passphrase, purposeMCP)
}

func cmdCheck(ctx context.Context, path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	st, err := newWorkerClient(cfg).Ping(ctx)
	if err != nil {
		return err
	}
	fmt.Println("Worker OK.")
	fmt.Printf("Linked: %v. Link state: %s. Last heartbeat: %s.\n", st.HasSession, orDash(st.State), orDash(st.LastHeartbeat))
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
