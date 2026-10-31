package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reladock/internal/platform"
)

func TestClientClaimsAndReportsUsingFixedControlPaths(t *testing.T) {
	task := platform.UpgradeClaim{ID: "11111111-1111-4111-8111-111111111111", TargetVersion: "v0.9.0", PreviousVersion: "v0.8.0", SHA256: strings.Repeat("a", 64), LeaseToken: strings.Repeat("b", 64)}
	var claims, reports int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		switch r.URL.Path {
		case "/agent/upgrade/claim":
			claims++
			var input struct {
				Arch           string `json:"arch"`
				CurrentVersion string `json:"currentVersion"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Arch != "amd64" || input.CurrentVersion != "v0.8.0" {
				t.Fatalf("claim body: %+v", input)
			}
			json.NewEncoder(w).Encode(task)
		case "/agent/upgrade/report":
			reports++
			var report platform.UpgradeReport
			if json.NewDecoder(r.Body).Decode(&report) != nil || report.ID != task.ID || report.State != "installing" {
				t.Fatalf("report body: %+v", report)
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := Client{HTTP: srv.Client(), BaseURL: srv.URL}
	claim, err := client.Claim(context.Background(), "amd64", "v0.8.0")
	if err != nil || claim == nil || *claim != task {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	if err = client.Report(context.Background(), platform.UpgradeReport{ID: task.ID, LeaseToken: task.LeaseToken, State: "installing"}); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || reports != 1 {
		t.Fatalf("requests claims=%d reports=%d", claims, reports)
	}
}

func TestClientNoTaskRedirectAndResponseBounds(t *testing.T) {
	mode := "empty"
	redirected := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "empty":
			w.WriteHeader(http.StatusNoContent)
		case "redirect":
			if r.URL.Path == "/redirected" {
				redirected = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "large":
			w.Write([]byte(strings.Repeat("x", 70<<10)))
		case "unknown":
			w.Write([]byte(`{"id":"11111111-1111-4111-8111-111111111111","targetVersion":"v0.9.0","previousVersion":"v0.8.0","sha256":"` + strings.Repeat("a", 64) + `","leaseToken":"` + strings.Repeat("b", 64) + `","extra":true}`))
		}
	}))
	defer srv.Close()
	client := Client{HTTP: srv.Client(), BaseURL: srv.URL}
	if claim, err := client.Claim(context.Background(), "amd64", "v0.8.0"); err != nil || claim != nil {
		t.Fatalf("empty claim: %+v %v", claim, err)
	}
	mode = "redirect"
	if _, err := client.Claim(context.Background(), "amd64", "v0.8.0"); err == nil || redirected {
		t.Fatalf("redirect followed=%v err=%v", redirected, err)
	}
	for _, invalidMode := range []string{"large", "unknown"} {
		mode = invalidMode
		if _, err := client.Claim(context.Background(), "amd64", "v0.8.0"); err == nil {
			t.Fatalf("%s response accepted", invalidMode)
		}
	}
}

func TestClientRejectsUnsafeInputsAndAssetURLIsFixed(t *testing.T) {
	client := Client{}
	for _, input := range []struct{ arch, version string }{{"386", "v0.8.0"}, {"amd64", "dev"}, {"amd64", "v0.8.0/../../x"}} {
		if _, err := client.Claim(context.Background(), input.arch, input.version); err == nil {
			t.Fatalf("unsafe claim accepted: %+v", input)
		}
	}
	url, err := ReleaseAssetURL("v0.9.0", "arm64")
	if err != nil || url != "https://github.com/LightToUs/reladock/releases/download/v0.9.0/reladock-v0.9.0-linux-arm64.tar.gz" {
		t.Fatalf("asset URL = %q, %v", url, err)
	}
	if _, err = ReleaseAssetURL("v0.9.0/evil", "amd64"); err == nil {
		t.Fatal("unsafe asset version accepted")
	}
}
