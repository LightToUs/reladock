package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"reladock/internal/agent"
	"reladock/internal/buildinfo"
	"reladock/internal/platform"
	"reladock/internal/updater"
)

func main() {
	if err := run(os.Args[1:], os.Geteuid()); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, euid int) error {
	if len(args) != 0 {
		return errors.New("reladock-updater accepts no arguments")
	}
	if euid != 0 {
		return errors.New("reladock-updater must run as root")
	}
	return execute(context.Background())
}

func readAgentEnvironment(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \t\r\n") || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("invalid Agent environment")
		}
		values[key] = value
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	for _, key := range []string{"RELADOCK_NODE", "RELADOCK_CONTROL", "RELADOCK_PKI"} {
		if values[key] == "" {
			return nil, fmt.Errorf("missing %s", key)
		}
	}
	return values, nil
}

func execute(ctx context.Context) error {
	values, err := readAgentEnvironment("/etc/reladock/agent.env")
	if err != nil {
		return errors.New("Agent environment unavailable")
	}
	credentials, err := agent.OpenCredentialManager(values["RELADOCK_PKI"], values["RELADOCK_NODE"])
	if err != nil {
		return errors.New("node credentials unavailable")
	}
	tlsConfig, err := credentials.TLSConfig()
	if err != nil {
		return err
	}
	controlTransport := &http.Transport{TLSClientConfig: tlsConfig}
	credentials.RegisterTransport(controlTransport)
	defer controlTransport.CloseIdleConnections()
	control := updater.Client{HTTP: &http.Client{Transport: controlTransport, Timeout: 20 * time.Second}, BaseURL: values["RELADOCK_CONTROL"]}
	downloadTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	defer downloadTransport.CloseIdleConnections()
	downloadClient := &http.Client{Transport: downloadTransport, Timeout: 5 * time.Minute}
	store, err := updater.OpenStateStore("/var/lib/reladock-updater")
	if err != nil {
		return err
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return errors.New("unsupported updater architecture")
	}
	stage := func(stageCtx context.Context, claim platform.UpgradeClaim) (updater.StagedBinaries, error) {
		taskRoot := filepath.Join("/var/lib/reladock-updater/tasks", claim.ID)
		if err := os.MkdirAll(taskRoot, 0700); err != nil {
			return updater.StagedBinaries{}, err
		}
		attempt, err := os.MkdirTemp(taskRoot, "stage-")
		if err != nil {
			return updater.StagedBinaries{}, err
		}
		archive := filepath.Join(attempt, "release.tar.gz")
		if err = updater.DownloadRelease(stageCtx, downloadClient, claim.TargetVersion, arch, archive); err != nil {
			return updater.StagedBinaries{}, err
		}
		return updater.VerifyArchive(archive, claim.SHA256, filepath.Join(attempt, "files"))
	}
	return updater.Run(ctx, updater.Options{Store: store, Control: control, Host: &updater.LocalHost{}, CurrentVersion: buildinfo.Current(), Arch: arch, HealthTimeout: updater.DefaultHealthTimeout, Stage: stage})
}
