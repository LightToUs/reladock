package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"reladock/internal/buildinfo"
	"reladock/internal/googleauth"
	"reladock/internal/nodeauth"
	"reladock/internal/notify"
	"reladock/internal/observability"
	"reladock/internal/payments"
	"reladock/internal/platform"
	"reladock/internal/runtimeconfig"
	"reladock/internal/tronpayments"
	"reladock/internal/web"
	assets "reladock/public"
)

func main() {
	checkOnly := flag.Bool("check-config", false, "validate runtime and TLS files without starting services or connecting to the database")
	flag.Parse()
	cfg, err := runtimeconfig.Load(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	var googleProvider googleauth.Provider
	if cfg.GoogleOIDC.Enabled() {
		googleProvider, err = googleauth.New(cfg.GoogleOIDC)
		if err != nil {
			log.Fatal("Google OIDC configuration invalid")
		}
	}
	paymentConfig, paymentEnabled, err := payments.ConfigFromEnv()
	if err != nil {
		log.Fatal("invalid optional payment configuration")
	}
	tronPaymentConfig, tronPaymentEnabled, err := tronpayments.ConfigFromEnv()
	if err != nil {
		log.Fatal("invalid optional TRON USDT payment configuration")
	}
	var tronPaymentClient *tronpayments.Client
	if tronPaymentEnabled {
		tronPaymentClient, err = tronpayments.NewClient(tronPaymentConfig)
		if err != nil {
			log.Fatal("TRON USDT payment connector invalid")
		}
	}
	notificationSender, err := notify.New(notify.Config{URL: os.Getenv("NOTIFICATION_WEBHOOK_URL"), Secret: os.Getenv("NOTIFICATION_WEBHOOK_SECRET")})
	if err != nil {
		log.Fatal("invalid optional incident notification configuration")
	}
	if cfg.CertFile != "" {
		if _, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err != nil {
			log.Fatal("invalid control TLS key pair")
		}
	}
	if os.Getenv("DATABASE_URL") == "" {
		log.Fatal("DATABASE_URL is required")
	}
	var store *platform.Store
	if !*checkOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		store, err = platform.OpenWithOptions(ctx, os.Getenv("DATABASE_URL"), platform.StoreOptions{AllowUnentitledRules: cfg.AllowUnentitledRules, SourceFlowRetentionDays: cfg.SourceFlowRetentionDays})
		cancel()
		if err != nil {
			log.Fatal("PostgreSQL startup failed; check DATABASE_URL and database availability")
		}
		defer store.Close()
	}
	if cfg.AgentTLSDir != "" {
		if cfg.ManagedPKIDir != "" && *checkOnly {
			origin, _ := url.Parse(cfg.Bootstrap.AgentOrigin)
			if err := nodeauth.ValidateManagedBundle(cfg.ManagedPKIDir, origin.Hostname()); err != nil {
				log.Fatalf("managed node PKI validation failed: %v", err)
			}
		}
		if cfg.ManagedPKIDir != "" && !*checkOnly {
			origin, _ := url.Parse(cfg.Bootstrap.AgentOrigin)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := store.EnsureManagedNodePKI(ctx, cfg.ManagedPKIDir, origin.Hostname())
			cancel()
			if err != nil {
				log.Fatalf("managed node PKI unavailable (existing identity was not replaced): %v", err)
			}
		}
		if _, err := nodeauth.ServerTLS(filepath.Join(cfg.AgentTLSDir, "ca.crt"), filepath.Join(cfg.AgentTLSDir, "server.crt"), filepath.Join(cfg.AgentTLSDir, "server.key")); err != nil {
			log.Fatal("invalid Agent server TLS files")
		}
	}
	var nodeSigner *nodeauth.BootstrapCA
	if cfg.Bootstrap.SignerEnabled() {
		signer, e := nodeauth.LoadBootstrapSigner(cfg.Bootstrap.CACert, cfg.Bootstrap.CAKey, cfg.Bootstrap.CAChain)
		if e != nil {
			log.Fatal("invalid node bootstrap signing CA")
		}
		nodeSigner = signer
	}
	var bootstrap *web.BootstrapOptions
	if cfg.Bootstrap.SignerEnabled() {
		bootstrap = &web.BootstrapOptions{Config: cfg.Bootstrap, Signer: nodeSigner}
	}
	if *checkOnly {
		if _, err := configureLightsailRuntime(context.Background(), true, cfg.Lightsail, nil, nil, defaultLightsailRuntimeFactories()); err != nil {
			log.Fatal("Lightsail connector startup failed")
		}
		log.Print("Runtime configuration and TLS files validated; no database connection or listener opened")
		return
	}
	metrics := observability.New(buildinfo.Current(), func(ctx context.Context) (platform.OperationalMetrics, error) {
		return store.OperationalMetrics(ctx, platform.DefaultContactStaleAfter)
	})
	if cfg.AllowUnentitledRules {
		log.Print("WARNING: development entitlement bypass enabled; rules may forward without an active plan, quota, or expiry")
	}
	var paymentHandler *web.Payments
	if paymentEnabled {
		provider, err := payments.NewStripe(paymentConfig)
		if err != nil {
			log.Fatal("payment connector invalid")
		}
		paymentHandler = web.NewPayments(store, provider, paymentConfig.WebhookSecret, cfg.Origin)
	}
	var tronPaymentHandler *web.TronPayments
	if tronPaymentEnabled {
		tronPaymentHandler = web.NewTronPayments(store, tronPaymentConfig.CollectionAddress)
	}
	lightsailStartup, lightsailCancel := context.WithTimeout(context.Background(), 15*time.Second)
	lightsailRuntime, err := configureLightsailRuntime(lightsailStartup, false, cfg.Lightsail, store, metrics, defaultLightsailRuntimeFactories())
	lightsailCancel()
	if err != nil {
		log.Fatal("Lightsail connector startup failed")
	}
	var lightsailWeb *web.LightsailOptions
	if lightsailRuntime != nil {
		lightsailWeb = lightsailRuntime.web
	}
	server := &http.Server{Addr: cfg.Address, Handler: web.NewWithOptions(store, assets.Files, cfg.Origin, web.Options{PersistentRateLimit: cfg.Production, Payments: paymentHandler, TronPayments: tronPaymentHandler, Bootstrap: bootstrap, Lightsail: lightsailWeb, GoogleAuth: googleProvider}), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if lightsailRuntime != nil {
		go lightsailRuntime.run(signals)
	}
	if tronPaymentEnabled {
		go runTronPayments(signals, store, tronPaymentClient, tronPaymentClient, tronPaymentConfig.CollectionAddress,
			func(runErr error) { _ = metrics.ObserveWorker(observability.WorkerTronPaymentConfirm, runErr) },
			func(runErr error) { _ = metrics.ObserveWorker(observability.WorkerTronPaymentScan, runErr) })
	}
	if notificationSender.Enabled() {
		go runNotifications(signals, store, notificationSender, metrics)
	}
	if cfg.SourceFlowRetentionDays != 0 {
		go runSourceFlowRetention(signals, 30*time.Second, 25*time.Second, store.PruneSourceFlows,
			func(runErr error) { _ = metrics.ObserveWorker(observability.WorkerSourceFlowRetention, runErr) }, func(message string) { log.Print(message) })
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			maintenance, done := context.WithTimeout(signals, 25*time.Second)
			_, subscriptionErr := store.EnforceExpiredSubscriptions(maintenance, 100)
			if subscriptionErr != nil && maintenance.Err() == nil {
				log.Print("subscription reconciliation failed")
			}
			_ = metrics.ObserveWorker(observability.WorkerSubscriptionReconcile, subscriptionErr)
			incidentErr := store.ReconcileIncidents(maintenance, platform.DefaultContactStaleAfter)
			if incidentErr != nil && maintenance.Err() == nil {
				log.Print("incident reconciliation failed")
			}
			_ = metrics.ObserveWorker(observability.WorkerIncidentReconcile, incidentErr)
			done()
			select {
			case <-signals.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	if cfg.Production {
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-signals.Done():
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(signals, 5*time.Second)
					if err := store.PruneRequestLimits(ctx); err != nil {
						log.Print("rate limit maintenance failed")
					}
					cancel()
				}
			}
		}()
	}
	type listener struct {
		server           *http.Server
		tls              bool
		cert, key, label string
	}
	listeners := []listener{{server: server, tls: cfg.CertFile != "", cert: cfg.CertFile, key: cfg.KeyFile, label: "control"}}
	if dir := cfg.AgentTLSDir; dir != "" {
		tlsConfig, e := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
		if e != nil {
			log.Fatal("Agent server TLS configuration invalid")
		}
		var renewalSigner platform.BootstrapSigner
		if nodeSigner != nil {
			renewalSigner = nodeSigner.Sign
		}
		listeners = append(listeners, listener{server: &http.Server{Addr: cfg.AgentAddress, Handler: web.NewAgent(store, renewalSigner), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}, tls: true, label: "agent"})
	}
	if cfg.MetricsAddress != "" {
		listeners = append(listeners, listener{server: &http.Server{Addr: cfg.MetricsAddress, Handler: metrics.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 * 1024}, label: "metrics"})
	}
	failures := make(chan error, len(listeners))
	for _, item := range listeners {
		go func(item listener) {
			if item.label == "agent" {
				log.Printf("Agent mTLS: https://%s (configuration, application receipts and metering)", item.server.Addr)
			} else if item.label == "metrics" {
				log.Printf("Operational metrics: http://%s/metrics (loopback only)", item.server.Addr)
			}
			if item.tls {
				failures <- item.server.ListenAndServeTLS(item.cert, item.key)
			} else {
				failures <- item.server.ListenAndServe()
			}
		}(item)
	}
	log.Printf("RelaDock: %s (listening on %s)", cfg.Origin, cfg.Address)
	var serveErr error
	select {
	case <-signals.Done():
	case serveErr = <-failures:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, item := range listeners {
		item.server.Shutdown(shutdown)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatal(serveErr)
	}
}
