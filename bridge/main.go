// texts-bridge links Google Messages (SMS, MMS and RCS) to a private
// Cloudflare Worker that serves the texts to Claude as a connector, and sends
// texts the user approved in Claude.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "dev"

const usage = `texts-bridge %s

Usage:
  texts-bridge           Set up (first time) and link Google Messages; same as double-clicking
  texts-bridge setup     Save the Worker address and passphrase on this device
  texts-bridge pair      Link Google Messages (run once, on your computer)
  texts-bridge run       Keep texts flowing to the Worker (run on your phone)
  texts-bridge url       Print the connector address to add to Claude
  texts-bridge check     Test the Worker address and passphrase
  texts-bridge version   Print the version

Options (put them after the command):
  -config PATH   Use this config file instead of the default
  -debug         Print detailed logs
`

func main() {
	// Double-clicking the program (no command) runs setup if needed, then pair.
	cmd := "guided"
	if len(os.Args) >= 2 {
		cmd = os.Args[1]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "config file path")
	debug := fs.Bool("debug", false, "print detailed logs")
	if len(os.Args) > 2 {
		_ = fs.Parse(os.Args[2:])
	}

	applyPlatformNetworkFixes()
	log := newLogger(*debug)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch cmd {
	case "guided":
		err = cmdGuided(ctx, *configPath, log, *debug)
	case "setup":
		err = cmdSetup(ctx, *configPath)
	case "pair":
		err = cmdPair(ctx, *configPath, log, *debug)
	case "run":
		err = cmdRun(ctx, *configPath, log, *debug)
	case "url":
		err = cmdURL(*configPath)
	case "check":
		err = cmdCheck(ctx, *configPath)
	case "version", "-v", "--version":
		fmt.Println(Version)
	case "help", "-h", "--help":
		fmt.Printf(usage, Version)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q.\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, Version)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// cmdGuided is what runs when the program is opened without a command, for
// example by double-clicking it on Windows. It keeps the window open at the end.
func cmdGuided(ctx context.Context, path string, log zerolog.Logger, debug bool) error {
	defer func() {
		fmt.Println()
		fmt.Println("Press Enter to close this window.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}()
	fmt.Printf("texts-bridge %s\n\n", Version)
	if _, err := loadConfig(path); err != nil {
		if err := cmdSetup(ctx, path); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return nil
		}
		fmt.Println()
	} else {
		fmt.Println("Using the saved Worker address and passphrase from", path)
		fmt.Println("(Run \"texts-bridge setup\" to change them.)")
		fmt.Println()
	}
	if err := cmdPair(ctx, path, log, debug); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	return nil
}
