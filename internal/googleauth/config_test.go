package googleauth

import (
	"strings"
	"testing"
)

func mapGet(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadConfigDisabledCompleteAndPartial(t *testing.T) {
	cfg, err := LoadConfig(mapGet(map[string]string{}), "https://console.example.com")
	if err != nil || cfg.Enabled() {
		t.Fatalf("disabled config = %+v, err = %v", cfg, err)
	}

	values := map[string]string{
		"GOOGLE_OIDC_CLIENT_ID":     "fixture.apps.googleusercontent.com",
		"GOOGLE_OIDC_CLIENT_SECRET": "fixture-secret",
	}
	cfg, err = LoadConfig(mapGet(values), "https://console.example.com")
	if err != nil || !cfg.Enabled() {
		t.Fatalf("enabled config = %+v, err = %v", cfg, err)
	}

	for _, tc := range []map[string]string{
		{"GOOGLE_OIDC_CLIENT_ID": values["GOOGLE_OIDC_CLIENT_ID"]},
		{"GOOGLE_OIDC_CLIENT_SECRET": values["GOOGLE_OIDC_CLIENT_SECRET"]},
		{"GOOGLE_OIDC_CLIENT_ID": "wrong.example.com", "GOOGLE_OIDC_CLIENT_SECRET": values["GOOGLE_OIDC_CLIENT_SECRET"]},
		{"GOOGLE_OIDC_CLIENT_ID": " fixture.apps.googleusercontent.com", "GOOGLE_OIDC_CLIENT_SECRET": values["GOOGLE_OIDC_CLIENT_SECRET"]},
		{"GOOGLE_OIDC_CLIENT_ID": values["GOOGLE_OIDC_CLIENT_ID"], "GOOGLE_OIDC_CLIENT_SECRET": "fixture\nsecret"},
	} {
		_, err = LoadConfig(mapGet(tc), "https://console.example.com")
		if err == nil {
			t.Fatalf("accepted invalid configuration: %#v", tc)
		}
		if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "fixture\nsecret") {
			t.Fatalf("configuration error leaked secret: %v", err)
		}
	}
}

func TestLoadConfigDerivesExactCallback(t *testing.T) {
	values := map[string]string{
		"GOOGLE_OIDC_CLIENT_ID":     "fixture.apps.googleusercontent.com",
		"GOOGLE_OIDC_CLIENT_SECRET": "fixture-secret",
	}
	cfg, err := LoadConfig(mapGet(values), "https://console.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.RedirectURL, "https://console.example.com:8443/api/auth/google/callback"; got != want {
		t.Fatalf("RedirectURL = %q, want %q", got, want)
	}
	for _, origin := range []string{
		"", "https://console.example.com/", "https://console.example.com/path",
		"https://user@console.example.com", "https://console.example.com?q=1",
		"https://console.example.com#fragment", "https://console.example.com\n",
	} {
		if _, err := LoadConfig(mapGet(values), origin); err == nil {
			t.Fatalf("accepted non-exact origin %q", origin)
		}
	}
}
