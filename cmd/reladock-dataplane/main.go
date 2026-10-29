// reladock-dataplane is intentionally separate from the validation Agent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"time"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
)

type dependencies struct {
	goos     string
	resolver dataplane.Resolver
	runner   func(string) (dataplane.Runner, error)
}

func run(ctx context.Context, args []string, out io.Writer, d dependencies) error {
	fs := flag.NewFlagSet("reladock-dataplane", flag.ContinueOnError)
	fs.SetOutput(out)
	config := fs.String("config", "", "platform.NodeConfig JSON file")
	node := fs.String("node", "", "expected node identity")
	snat := fs.String("snat", "", "explicit source NAT IPv4")
	apply := fs.Bool("apply", false, "opt in to Linux nft application")
	dry := fs.Bool("dry-run", false, "only compile the plan (default)")
	recoverState := fs.Bool("recover", false, "restore pending last-good state; requires --apply")
	stateDir := fs.String("state-dir", "", "absolute private state directory; required for apply")
	nft := fs.String("nft", "/usr/sbin/nft", "trusted absolute nft binary")
	timeout := fs.Duration("timeout", 30*time.Second, "compile/apply timeout")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *apply && *dry {
		return errors.New("--apply and --dry-run conflict")
	}
	if *recoverState && !*apply {
		return errors.New("--recover requires --apply")
	}
	if *node == "" || *snat == "" || (!*recoverState && *config == "") {
		return errors.New("--node, --snat and --config are required (recovery does not require config)")
	}
	if *recoverState && *config != "" {
		return errors.New("recovery and config application must be separate invocations")
	}
	if *timeout <= 0 || *timeout > 5*time.Minute {
		return errors.New("timeout must be positive and at most 5m")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	opts := dataplane.Options{NodeID: *node, SNATAddress: *snat, Resolver: d.resolver}
	var cfg platform.NodeConfig
	if !*recoverState {
		f, e := os.Open(*config)
		if e != nil {
			return e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
			return errors.New("config must be a regular file at most 2 MiB")
		}
		dec := json.NewDecoder(io.LimitReader(f, 2*1024*1024+1))
		dec.DisallowUnknownFields()
		if e = dec.Decode(&cfg); e != nil {
			return e
		}
		if dec.Decode(new(any)) != io.EOF {
			return errors.New("trailing config JSON")
		}
	}
	if !*apply {
		p, e := dataplane.Compile(ctx, cfg, opts)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintf(out, "# PLAN ONLY: no nft commands executed; bandwidth is NOT enforced; path health unknown.\n%s", p.Script)
		return e
	}
	if d.goos != "linux" {
		return errors.New("--apply is supported only on Linux")
	}
	if *stateDir == "" {
		return errors.New("--state-dir is required for --apply")
	}
	if d.runner == nil {
		return errors.New("runner dependency missing")
	}
	r, e := d.runner(*nft)
	if e != nil {
		return e
	}
	store, e := dataplane.NewFileStore(*stateDir)
	if e != nil {
		return e
	}
	a, e := dataplane.NewAdapter(opts, r, store, store.Locker(), dataplane.ReadbackVerifier(r))
	if e != nil {
		return e
	}
	if *recoverState {
		if e = a.Recover(ctx); e != nil {
			return e
		}
		_, e = fmt.Fprintln(out, "Local recovery completed; path health unknown; no platform ACK.")
		return e
	}
	p, e := a.Apply(ctx, cfg)
	if e != nil {
		return e
	}
	_, e = fmt.Fprintf(out, "Local nft transaction/readback completed for generation %d; bandwidth is NOT enforced; path health unknown; no platform ACK.\n", p.Generation)
	return e
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	d := dependencies{goos: runtime.GOOS, resolver: net.DefaultResolver, runner: func(path string) (dataplane.Runner, error) { return dataplane.NewExecRunner(path) }}
	if e := run(ctx, os.Args[1:], os.Stdout, d); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
