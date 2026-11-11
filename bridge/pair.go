package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
)

const signInURL = "https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config"

func cmdPair(ctx context.Context, path string, log zerolog.Logger, debug bool) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	wc := newWorkerClient(cfg)
	if _, err := wc.Ping(ctx); err != nil {
		return fmt.Errorf("checking the Worker first: %w", err)
	}

	fmt.Println("Link Google Messages")
	fmt.Println()
	fmt.Println("1. Open a private window (Ctrl+Shift+N in Chrome or Edge).")
	fmt.Println("2. Open this address and sign in with the Google account your phone's Google Messages uses:")
	fmt.Println("   " + signInURL)
	fmt.Println("3. Press F12, open the Network tab, then press F5 to reload the page.")
	fmt.Println("4. Right-click the request named config, then Copy > Copy as cURL (bash).")
	fmt.Println("5. Come back here and press Enter. (If that doesn't work, paste it here and press Enter twice.)")
	fmt.Println()

	cookies, err := readCookies()
	if err != nil {
		return err
	}
	fmt.Println("Got the cookies. Asking your phone to confirm...")

	libLog := log.With().Str("component", "libgm").Logger()
	if !debug {
		libLog = libLog.Level(zerolog.WarnLevel)
	}
	ad := libgm.NewAuthData()
	ad.SetCookies(cookies)
	client := libgm.NewClient(ad, nil, libLog, httpSettings())
	client.SetEventHandler(func(evt any) {
		log.Debug().Type("event_type", evt).Msg("Event during pairing")
	})
	if err := client.FetchConfig(ctx); err != nil {
		return fmt.Errorf("Google did not accept the cookies (sign in again in a fresh private window): %w", err)
	}
	bgCtx, cancelBg := context.WithCancel(ctx)
	defer cancelBg()
	emoji, sess, err := client.StartGaiaPairing(ctx, bgCtx)
	if err != nil {
		client.Disconnect()
		switch {
		case errors.Is(err, libgm.ErrNoDevicesFound):
			return errors.New("Google found no phone with Google Messages on this account. Check that Google Messages on your phone is signed in to the same Google account (Messages > profile picture > Messages for web / Device pairing)")
		case errors.Is(err, libgm.ErrPairingInitTimeout):
			return errors.New("your phone did not answer. Open Google Messages on your phone, make sure it has internet, and run pair again")
		case errors.Is(err, events.ErrCallerNoPermission):
			return errors.New("Google refused the pairing for this account (no permission). Try again with a fresh private window")
		}
		return fmt.Errorf("starting the pairing: %w", err)
	}
	fmt.Println()
	fmt.Println("==============================================")
	fmt.Printf("   On your phone, tap this emoji:   %s\n", emoji)
	fmt.Println("==============================================")
	fmt.Println("Google Messages on your phone is showing a few emoji. Tap the one above.")
	fmt.Println("Waiting up to a few minutes...")

	if _, err := client.FinishGaiaPairing(ctx, sess); err != nil {
		client.Disconnect()
		switch {
		case errors.Is(err, libgm.ErrIncorrectEmoji):
			return errors.New("a different emoji was tapped. Run pair again")
		case errors.Is(err, libgm.ErrPairingCancelled):
			return errors.New("the pairing was cancelled on the phone. Run pair again")
		case errors.Is(err, libgm.ErrPairingTimeout):
			return errors.New("the phone took too long. Run pair again and tap the emoji sooner")
		}
		return fmt.Errorf("finishing the pairing: %w", err)
	}
	// The same pairing will run on the phone, so this computer lets go of it now.
	client.Disconnect()
	cancelBg()
	// Give Google a moment to settle the new pairing before the phone connects.
	time.Sleep(2 * time.Second)

	store := newSessionStore(cfg, wc, log)
	if _, err := store.Save(ctx, client.AuthData, true); err != nil {
		return fmt.Errorf("the pairing worked, but saving it to the Worker failed: %w. Run pair again", err)
	}
	// This computer does not need its own copy.
	store.RemoveLocal()

	fmt.Println()
	fmt.Println("Linked. The pairing is saved, encrypted, in your Worker.")
	fmt.Println("Close the private window now. Do not sign out in it first: signing out would undo the link.")
	fmt.Println()
	fmt.Println("If texts-bridge is already running on your phone, it picks up the new link within 2 minutes.")
	fmt.Println()
	fmt.Println("Your connector address for Claude (keep it private, like a password):")
	fmt.Println(connectorURL(cfg))
	return nil
}

// readCookies takes the copied request from the clipboard when it can, since
// pasting thousands of characters into a terminal is unreliable on Windows.
func readCookies() (map[string]string, error) {
	input, err := readPastedBlock(os.Stdin)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input) != "" {
		return parseCookieInput(input)
	}
	clip, cerr := readClipboard()
	if cerr != nil || strings.TrimSpace(clip) == "" {
		fmt.Println("Could not read the clipboard. Paste the copied request here, then press Enter twice:")
		input, err = readPastedBlock(os.Stdin)
		if err != nil {
			return nil, err
		}
		return parseCookieInput(input)
	}
	return parseCookieInput(clip)
}

func readClipboard() (string, error) {
	var cmds [][]string
	switch runtime.GOOS {
	case "windows":
		cmds = [][]string{{"powershell", "-NoProfile", "-NonInteractive", "-Command", "Get-Clipboard -Raw"}}
	case "darwin":
		cmds = [][]string{{"pbpaste"}}
	case "android":
		cmds = [][]string{{"termux-clipboard-get"}}
	default:
		cmds = [][]string{{"wl-paste", "--no-newline"}, {"xclip", "-selection", "clipboard", "-o"}, {"xsel", "--clipboard", "--output"}}
	}
	var lastErr error
	for _, c := range cmds {
		out, err := exec.Command(c[0], c[1:]...).Output()
		if err == nil {
			return string(out), nil
		}
		lastErr = err
	}
	return "", lastErr
}

// readPastedBlock reads lines until an empty line, or until EOF. An empty first
// line returns "" so the caller can use the clipboard instead.
func readPastedBlock(f *os.File) (string, error) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var b strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return b.String(), nil
}
