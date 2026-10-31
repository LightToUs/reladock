package main

import (
	"strings"
	"testing"
)

func TestParseControlCreateUsesSafeDefaults(t *testing.T) {
	c, err := parseArgs([]string{"control-create", "--destination", "/backup", "--recipient", "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqp5e5z"}, func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://local/db"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ageBinary != "/usr/bin/age" || c.pgDumpBinary != "/usr/bin/pg_dump" || c.databaseURL == "" {
		t.Fatalf("unsafe defaults: %+v", c)
	}
}

func TestParseRejectsMissingAndUnknownOptionsWithoutSecretEcho(t *testing.T) {
	secret := "postgres://u:super-secret@db/x"
	_, err := parseArgs([]string{"control-create", "--unknown", "x"}, func(string) string { return secret })
	if err == nil {
		t.Fatal("accepted unknown option")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("secret in error: %v", err)
	}
	if _, err = parseArgs([]string{"node-verify", "--bundle", "/tmp/a.age"}, func(string) string { return "" }); err == nil {
		t.Fatal("accepted missing identity and staging")
	}
}

func TestParseNodeCreateFixedOperationalDefaults(t *testing.T) {
	c, err := parseArgs([]string{"node-create", "--destination", "/backup", "--recipient", "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqp5e5z"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.systemctlBinary != "/usr/bin/systemctl" || c.controlLock != "/var/lib/reladock-locks/control.lock" {
		t.Fatalf("defaults: %+v", c)
	}
}
