// Local operator tooling. No tenant-accessible route grants these privileges.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"reladock/internal/nodeauth"
	"reladock/internal/platform"
	"reladock/internal/runtimeconfig"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("commands: platform-admin | node-add | maintenance | enroll | retry | dev-enroll | wallet-adjust")
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	id := flags.String("id", "hk-01", "node ID")
	email := flags.String("email", "", "existing account email for explicit platform grant/revoke")
	name := flags.String("name", "", "node name")
	region := flags.String("region", "Hong Kong", "region")
	address := flags.String("listen-address", "0.0.0.0", "planned bind IP")
	enabled := flags.Bool("enabled", true, "maintenance enabled")
	certPath := flags.String("cert", "", "client certificate PEM")
	dir := flags.String("dir", ".local/pki", "new local development PKI directory")
	tenantID := flags.String("tenant", "", "tenant ID for a local wallet correction")
	ruleID := flags.String("rule", "", "optional rule ID for wallet correction attribution")
	amount := flags.Int64("amount-cents", 0, "signed integer USD cents; local correction only, not a payment")
	key := flags.String("key", "", "unique idempotency key for wallet correction")
	reason := flags.String("reason", "", "mandatory reason for wallet correction")
	operator := flags.String("operator", "", "operator name recorded in immutable ledger")
	acknowledge := flags.Bool("acknowledge-wallet-adjustment", false, "explicitly authorize a local wallet correction, not a verified payment")
	flags.Parse(os.Args[2:])
	if err := validateOperatorFlags(command, flags); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	allowUnentitled, err := runtimeconfig.LoadDevelopmentEntitlementBypass(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	store, err := platform.OpenWithOptions(ctx, os.Getenv("DATABASE_URL"), platform.StoreOptions{AllowUnentitledRules: allowUnentitled})
	if err != nil {
		log.Fatal("database unavailable; check local configuration")
	}
	defer store.Close()
	switch command {
	case "wallet-adjust":
		if !*acknowledge {
			log.Fatal("wallet adjustment requires --acknowledge-wallet-adjustment and must not represent an unverified payment")
		}
		entry, e := store.AdjustWalletFromCLI(ctx, platform.WalletAdjustment{TenantID: *tenantID, RuleID: *ruleID, Key: *key, AmountCents: *amount, Reason: *reason, Operator: *operator})
		if e != nil {
			log.Fatal(e)
		}
		json.NewEncoder(os.Stdout).Encode(entry)
		return
	case "platform-admin":
		err = store.SetPlatformAdmin(ctx, *email, *enabled)
	case "node-add":
		err = store.AddNode(ctx, *id, *name, *region, *address)
	case "maintenance":
		err = store.MaintainNode(ctx, *id, *enabled)
	case "retry":
		err = store.RetryNodeConfig(ctx, *id)
	case "dev-enroll":
		if err = os.MkdirAll(filepath.Dir(*dir), 0700); err == nil {
			err = nodeauth.CreateLocalBundle(*dir, *id)
		}
		if err != nil {
			break
		}
		*certPath = filepath.Join(*dir, "agent.crt")
		fallthrough
	case "enroll":
		cert, e := nodeauth.ReadCertificate(*certPath)
		if e != nil {
			err = e
			break
		}
		nodeID, e := nodeauth.NodeIdentity(cert)
		if e != nil || nodeID != *id || cert.IsCA || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			err = fmt.Errorf("certificate does not identify a valid node %s", *id)
			break
		}
		err = store.EnrollNode(ctx, *id, nodeauth.Fingerprint(cert))
	default:
		log.Fatal("unknown operator command")
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Node operator action completed. No network rules were applied.")
}

func validateOperatorFlags(command string, flags *flag.FlagSet) error {
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments; use --enabled=false or --enabled=true")
	}
	if command == "platform-admin" || command == "maintenance" {
		explicit := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "enabled" {
				explicit = true
			}
		})
		if !explicit {
			return fmt.Errorf("%s requires explicit --enabled=true or --enabled=false", command)
		}
	}
	return nil
}
