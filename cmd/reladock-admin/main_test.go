package main

import (
	"flag"
	"io"
	"testing"
)

func TestPrivilegeFlagsRequireExplicitUnambiguousBoolean(t *testing.T) {
	for _, args := range [][]string{{"--email", "x@example.com"}, {"--email", "x@example.com", "--enabled", "false"}, {"--email", "x@example.com", "--enabled=false", "unexpected"}} {
		f := flag.NewFlagSet("platform-admin", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		f.String("email", "", "")
		f.Bool("enabled", true, "")
		if err := f.Parse(args); err != nil {
			t.Fatal(err)
		}
		if validateOperatorFlags("platform-admin", f) == nil {
			t.Fatalf("unsafe args accepted: %v", args)
		}
	}
	f := flag.NewFlagSet("platform-admin", flag.ContinueOnError)
	f.Bool("enabled", true, "")
	if err := f.Parse([]string{"--enabled=false"}); err != nil {
		t.Fatal(err)
	}
	if err := validateOperatorFlags("platform-admin", f); err != nil {
		t.Fatal(err)
	}
}
