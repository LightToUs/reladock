package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"reladock/internal/recovery"
)

type command struct{ mode, destination, recipient, bundle, identity, staging, databaseURL, ageBinary, pgDumpBinary, pgRestoreBinary, systemctlBinary, controlLock string }

func parseArgs(args []string, getenv func(string) string) (command, error) {
	var c command
	if len(args) == 0 {
		return c, errors.New("expected control-create, control-verify, node-create or node-verify")
	}
	c.mode = args[0]
	set := flag.NewFlagSet(c.mode, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&c.ageBinary, "age-binary", "/usr/bin/age", "")
	switch c.mode {
	case "control-create":
		set.StringVar(&c.destination, "destination", "", "")
		set.StringVar(&c.recipient, "recipient", "", "")
		set.StringVar(&c.pgDumpBinary, "pg-dump-binary", "/usr/bin/pg_dump", "")
		c.databaseURL = getenv("DATABASE_URL")
	case "control-verify":
		set.StringVar(&c.bundle, "bundle", "", "")
		set.StringVar(&c.identity, "identity", "", "")
		set.StringVar(&c.pgRestoreBinary, "pg-restore-binary", "/usr/bin/pg_restore", "")
		c.databaseURL = getenv("DATABASE_URL")
	case "node-create":
		set.StringVar(&c.destination, "destination", "", "")
		set.StringVar(&c.recipient, "recipient", "", "")
		set.StringVar(&c.systemctlBinary, "systemctl-binary", "/usr/bin/systemctl", "")
		set.StringVar(&c.controlLock, "control-lock", "/var/lib/reladock-locks/control.lock", "")
	case "node-verify":
		set.StringVar(&c.bundle, "bundle", "", "")
		set.StringVar(&c.identity, "identity", "", "")
		set.StringVar(&c.staging, "staging", "", "")
	default:
		return c, errors.New("unknown recovery command")
	}
	if err := set.Parse(args[1:]); err != nil {
		return c, errors.New("invalid recovery option")
	}
	if set.NArg() != 0 {
		return c, errors.New("unexpected positional argument")
	}
	switch c.mode {
	case "control-create":
		if c.destination == "" || c.recipient == "" || c.databaseURL == "" {
			return c, errors.New("control-create requires destination, recipient and DATABASE_URL")
		}
	case "control-verify":
		if c.bundle == "" || c.identity == "" || c.databaseURL == "" {
			return c, errors.New("control-verify requires bundle, identity and DATABASE_URL")
		}
	case "node-create":
		if c.destination == "" || c.recipient == "" {
			return c, errors.New("node-create requires destination and recipient")
		}
	case "node-verify":
		if c.bundle == "" || c.identity == "" || c.staging == "" {
			return c, errors.New("node-verify requires bundle, identity and staging")
		}
	}
	return c, nil
}

func printBundle(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	fmt.Printf("Encrypted recovery bundle: %s\nSize: %d bytes\nSHA256: %s\n", path, size, hex.EncodeToString(h.Sum(nil)))
	return nil
}

func execute(ctx context.Context, c command) error {
	switch c.mode {
	case "control-create":
		p, err := recovery.CreateControl(ctx, recovery.ControlCreateConfig{DatabaseURL: c.databaseURL, Destination: c.destination, Recipient: c.recipient, AgeBinary: c.ageBinary, PGDumpBinary: c.pgDumpBinary})
		if err != nil {
			return err
		}
		return printBundle(p)
	case "control-verify":
		if err := recovery.VerifyControl(ctx, recovery.ControlVerifyConfig{DatabaseURL: c.databaseURL, Bundle: c.bundle, Identity: c.identity, AgeBinary: c.ageBinary, PGRestoreBinary: c.pgRestoreBinary}); err != nil {
			return err
		}
		fmt.Println("Control recovery bundle verified in an isolated temporary database; live state was not modified.")
		return nil
	case "node-create":
		p, err := recovery.CreateNode(ctx, recovery.NodeCreateConfig{Destination: c.destination, Recipient: c.recipient, AgeBinary: c.ageBinary, SystemctlBinary: c.systemctlBinary, ControlLock: c.controlLock})
		if err != nil {
			return err
		}
		return printBundle(p)
	case "node-verify":
		m, err := recovery.VerifyNode(ctx, recovery.NodeVerifyConfig{Bundle: c.bundle, Identity: c.identity, AgeBinary: c.ageBinary, Staging: c.staging})
		if err != nil {
			return err
		}
		fmt.Printf("Node recovery bundle verified and staged at %s (source %s); live state was not modified.\n", c.staging, m.SourceID)
		return nil
	default:
		return errors.New("unknown recovery command")
	}
}

func main() {
	c, err := parseArgs(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reladock-recovery:", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = execute(ctx, c); err != nil {
		fmt.Fprintln(os.Stderr, "reladock-recovery:", err)
		os.Exit(1)
	}
}
