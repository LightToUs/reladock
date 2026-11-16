package googleauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type providerTestServer struct {
	server       *httptest.Server
	key          *rsa.PrivateKey
	issuer       string
	mu           sync.Mutex
	claims       map[string]any
	status       int
	body         string
	redirect     bool
	redirectSeen bool
	verifier     string
}

func newProviderTestServer(t *testing.T) *providerTestServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &providerTestServer{key: key, status: http.StatusOK}
	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(mux)
	fixture.issuer = fixture.server.URL
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		e := big.NewInt(int64(key.PublicKey.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "fixture-key", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	})
	mux.HandleFunc("/redirected", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		fixture.redirectSeen = true
		fixture.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		fixture.verifier = r.Form.Get("code_verifier")
		status, body, redirect := fixture.status, fixture.body, fixture.redirect
		claims := cloneClaims(fixture.claims)
		fixture.mu.Unlock()
		if redirect {
			http.Redirect(w, r, "/redirected", http.StatusFound)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		idToken := signProviderTestToken(t, key, claims)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fixture-access-token",
			"id_token":     idToken,
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	})
	t.Cleanup(fixture.server.Close)
	return fixture
}

func cloneClaims(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func (f *providerTestServer) validClaims(nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": f.issuer, "aud": "fixture.apps.googleusercontent.com",
		"sub": "google-subject-123", "email": "Member@Example.com",
		"email_verified": true, "nonce": nonce,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

func (f *providerTestServer) provider(t *testing.T) Provider {
	t.Helper()
	provider, err := newProvider(Config{
		ClientID: "fixture.apps.googleusercontent.com", ClientSecret: "fixture-secret",
		RedirectURL: "https://console.example.com/api/auth/google/callback",
	}, providerEndpoints{
		Issuer: f.issuer, AuthorizationURL: f.server.URL + "/authorize",
		TokenURL: f.server.URL + "/token", JWKSURL: f.server.URL + "/keys",
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func signProviderTestToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(payload)
	}
	signingInput := encode(map[string]string{"alg": "RS256", "kid": "fixture-key", "typ": "JWT"}) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestProviderAuthorizationURLUsesPKCEAndNoSecret(t *testing.T) {
	fixture := newProviderTestServer(t)
	provider := fixture.provider(t)
	verifier := "fixture-verifier-with-enough-entropy-123456789012345678901234"
	rawURL := provider.AuthCodeURL("fixture-state", "fixture-nonce", verifier)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	digest := sha256.Sum256([]byte(verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	for key, want := range map[string]string{
		"response_type": "code",
		"client_id":     "fixture.apps.googleusercontent.com",
		"redirect_uri":  "https://console.example.com/api/auth/google/callback",
		"scope":         "openid email", "state": "fixture-state", "nonce": "fixture-nonce",
		"code_challenge": wantChallenge, "code_challenge_method": "S256",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(rawURL, "fixture-secret") {
		t.Fatal("authorization URL leaked client secret")
	}
}

func TestProviderExchangeValidatesOIDCClaims(t *testing.T) {
	const nonce = "fixture-nonce"
	const verifier = "fixture-verifier-with-enough-entropy-123456789012345678901234"
	base := newProviderTestServer(t)

	t.Run("valid", func(t *testing.T) {
		base.mu.Lock()
		base.status, base.body, base.redirect = http.StatusOK, "", false
		base.claims = base.validClaims(nonce)
		base.mu.Unlock()
		claims, err := base.provider(t).Exchange(context.Background(), "fixture-code", verifier, nonce)
		if err != nil {
			t.Fatal(err)
		}
		if claims.Subject != "google-subject-123" || claims.Email != "Member@Example.com" {
			t.Fatalf("claims = %+v", claims)
		}
		base.mu.Lock()
		gotVerifier := base.verifier
		base.mu.Unlock()
		if gotVerifier != verifier {
			t.Fatalf("code_verifier = %q, want %q", gotVerifier, verifier)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong-audience", func(c map[string]any) { c["aud"] = "other.apps.googleusercontent.com" }},
		{"wrong-issuer", func(c map[string]any) { c["iss"] = "https://issuer.invalid" }},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"wrong-nonce", func(c map[string]any) { c["nonce"] = "wrong" }},
		{"missing-subject", func(c map[string]any) { delete(c, "sub") }},
		{"unverified-email", func(c map[string]any) { c["email_verified"] = false }},
		{"missing-email", func(c map[string]any) { delete(c, "email") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := base.validClaims(nonce)
			tc.mutate(claims)
			base.mu.Lock()
			base.status, base.body, base.redirect, base.claims = http.StatusOK, "", false, claims
			base.mu.Unlock()
			_, err := base.provider(t).Exchange(context.Background(), "fixture-code", verifier, nonce)
			if err == nil {
				t.Fatal("accepted invalid ID token")
			}
			if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "fixture-access-token") {
				t.Fatalf("error leaked provider secret: %v", err)
			}
		})
	}
}

func TestProviderExchangeRejectsUnsafeResponses(t *testing.T) {
	const nonce = "fixture-nonce"
	const verifier = "fixture-verifier-with-enough-entropy-123456789012345678901234"
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		redirect bool
	}{
		{"non-200", http.StatusBadGateway, "provider-body-secret", false},
		{"oversized", http.StatusOK, strings.Repeat("x", 2<<20), false},
		{"redirect", http.StatusOK, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newProviderTestServer(t)
			fixture.mu.Lock()
			fixture.status, fixture.body, fixture.redirect = tc.status, tc.body, tc.redirect
			fixture.claims = fixture.validClaims(nonce)
			fixture.mu.Unlock()
			_, err := fixture.provider(t).Exchange(context.Background(), "fixture-code", verifier, nonce)
			if err == nil {
				t.Fatal("accepted unsafe token response")
			}
			message := err.Error()
			for _, forbidden := range []string{"fixture-secret", "provider-body-secret", "fixture-access-token", verifier, nonce} {
				if strings.Contains(message, forbidden) {
					t.Fatalf("error leaked %q: %v", forbidden, err)
				}
			}
			if tc.redirect {
				fixture.mu.Lock()
				seen := fixture.redirectSeen
				fixture.mu.Unlock()
				if seen {
					t.Fatal("HTTP client followed token endpoint redirect")
				}
			}
		})
	}
}

func TestProviderNewRejectsDisabledOrInvalidConfigWithoutLeakingSecret(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{ClientID: "fixture.apps.googleusercontent.com", ClientSecret: "fixture-secret"},
		{ClientID: "wrong.example.com", ClientSecret: "fixture-secret", RedirectURL: "https://console.example.com/api/auth/google/callback"},
	} {
		_, err := New(cfg)
		if err == nil {
			t.Fatalf("New accepted invalid config: %+v", cfg)
		}
		if strings.Contains(fmt.Sprint(err), "fixture-secret") {
			t.Fatalf("New error leaked secret: %v", err)
		}
	}
}
