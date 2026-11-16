package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"reladock/internal/googleauth"
	"reladock/internal/platform"
)

type fakeGoogleProvider struct {
	mu            sync.Mutex
	state         string
	nonce         string
	verifier      string
	exchangeCode  string
	exchangeCalls int
	claims        googleauth.Claims
	exchangeErr   error
}

func (p *fakeGoogleProvider) AuthCodeURL(state, nonce, verifier string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state, p.nonce, p.verifier = state, nonce, verifier
	return "https://accounts.google.com/o/oauth2/v2/auth?state=" + url.QueryEscape(state)
}

func (p *fakeGoogleProvider) Exchange(_ context.Context, code, verifier, nonce string) (googleauth.Claims, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exchangeCode = code
	p.exchangeCalls++
	if verifier != p.verifier || nonce != p.nonce {
		return googleauth.Claims{}, errors.New("fake nonce or verifier mismatch")
	}
	return p.claims, p.exchangeErr
}

func googleAuthRequest(handler http.Handler, method, rawURL, body, remoteAddr string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, rawURL, strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "RelaDock")
	parsed, _ := url.Parse(rawURL)
	req.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestGoogleAuthStatusAndStartAreOptionalSameOriginAndBounded(t *testing.T) {
	store := testWebStore(t)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("google auth")}}
	disabled := NewWithOptions(store, assets, "https://console.example.com", Options{})
	status := googleAuthRequest(disabled, http.MethodGet, "https://console.example.com/api/auth/google/status", "", "192.0.2.1:1234", nil)
	if status.Code != http.StatusOK || status.Body.String() != "{\"enabled\":false}\n" {
		t.Fatalf("disabled status: %d %s", status.Code, status.Body.String())
	}
	if start := googleAuthRequest(disabled, http.MethodPost, "https://console.example.com/api/auth/google/start", `{"returnTo":"/"}`, "192.0.2.2:1234", nil); start.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled start: %d %s", start.Code, start.Body.String())
	}

	provider := &fakeGoogleProvider{}
	handler := NewWithOptions(store, assets, "https://console.example.com", Options{GoogleAuth: provider})
	for name, mutate := range map[string]func(*http.Request){
		"missing requested-with": func(request *http.Request) { request.Header.Del("X-Requested-With") },
		"cross-site origin":      func(request *http.Request) { request.Header.Set("Origin", "https://evil.example") },
		"cross-site fetch":       func(request *http.Request) { request.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://console.example.com/api/auth/google/start", strings.NewReader(`{"returnTo":"/"}`))
			request.RemoteAddr = "192.0.2.3:1234"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Requested-With", "RelaDock")
			request.Header.Set("Origin", "https://console.example.com")
			mutate(request)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	for name, body := range map[string]string{
		"open redirect":  `{"returnTo":"https://evil.example"}`,
		"unknown field":  `{"returnTo":"/","extra":true}`,
		"missing target": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := googleAuthRequest(handler, http.MethodPost, "https://console.example.com/api/auth/google/start", body, "192.0.2.4:1234", nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	started := googleAuthRequest(handler, http.MethodPost, "https://console.example.com/api/auth/google/start", `{"returnTo":"/admin"}`, "192.0.2.5:1234", nil)
	if started.Code != http.StatusOK {
		t.Fatalf("start: %d %s", started.Code, started.Body.String())
	}
	var payload struct {
		AuthorizationURL string `json:"authorizationURL"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	state, nonce, verifier := provider.state, provider.nonce, provider.verifier
	provider.mu.Unlock()
	if len(state) != 43 || len(nonce) != 43 || len(verifier) != 43 {
		t.Fatalf("generated lengths state=%d nonce=%d verifier=%d", len(state), len(nonce), len(verifier))
	}
	if payload.AuthorizationURL != "https://accounts.google.com/o/oauth2/v2/auth?state="+url.QueryEscape(state) {
		t.Fatalf("authorization URL = %q", payload.AuthorizationURL)
	}
	stateCookie := findCookie(started.Result().Cookies(), googleStateCookieName)
	if stateCookie == nil || stateCookie.Value != state || stateCookie.Path != googleCallbackPath || !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || !stateCookie.Secure || stateCookie.MaxAge != 600 {
		t.Fatalf("state cookie = %+v", stateCookie)
	}
	if strings.Contains(started.Body.String(), nonce) || strings.Contains(started.Body.String(), verifier) {
		t.Fatal("start response leaked nonce or verifier")
	}

	rateProvider := &fakeGoogleProvider{}
	rateHandler := NewWithOptions(store, assets, "https://console.example.com", Options{GoogleAuth: rateProvider})
	for attempt := 0; attempt < 10; attempt++ {
		recorder := googleAuthRequest(rateHandler, http.MethodPost, "https://console.example.com/api/auth/google/start", `{"returnTo":"/"}`, "192.0.2.6:1234", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("rate attempt %d: %d %s", attempt, recorder.Code, recorder.Body.String())
		}
	}
	limited := googleAuthRequest(rateHandler, http.MethodPost, "https://console.example.com/api/auth/google/start", `{"returnTo":"/"}`, "192.0.2.6:1234", nil)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate limit: %d %s", limited.Code, limited.Body.String())
	}
}

func googleAuthTestStore(t *testing.T) (*platform.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("google_web_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := platform.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		inspection.Close()
		store.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return store, inspection
}

func newGoogleAuthHandler(store *platform.Store, provider *fakeGoogleProvider) http.Handler {
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("google auth")}}
	return NewWithOptions(store, assets, "https://console.example.com", Options{GoogleAuth: provider})
}

func startGoogleAuth(t *testing.T, handler http.Handler, provider *fakeGoogleProvider, returnTo, remoteAddr string) *http.Cookie {
	t.Helper()
	recorder := googleAuthRequest(handler, http.MethodPost, "https://console.example.com/api/auth/google/start", `{"returnTo":"`+returnTo+`"}`, remoteAddr, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start: %d %s", recorder.Code, recorder.Body.String())
	}
	cookie := findCookie(recorder.Result().Cookies(), googleStateCookieName)
	if cookie == nil {
		t.Fatal("start did not set Google state cookie")
	}
	provider.mu.Lock()
	state := provider.state
	provider.mu.Unlock()
	if cookie.Value != state {
		t.Fatalf("cookie state = %q, provider state = %q", cookie.Value, state)
	}
	return cookie
}

func googleCallback(handler http.Handler, rawQuery, remoteAddr string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "https://console.example.com"+googleCallbackPath+"?"+rawQuery, nil)
	request.RemoteAddr = remoteAddr
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func completeGoogleLinkHTTP(handler http.Handler, email, password, remoteAddr string, cookie *http.Cookie) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return googleAuthRequest(handler, http.MethodPost, "https://console.example.com"+googleLinkPath, string(body), remoteAddr, cookie)
}

func assertGoogleCallbackFailure(t *testing.T, recorder *httptest.ResponseRecorder, returnTo string, forbidden ...string) {
	t.Helper()
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("failure status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	wantLocation := returnTo + "?googleAuth=failed"
	if got := recorder.Header().Get("Location"); got != wantLocation {
		t.Fatalf("failure location = %q, want %q", got, wantLocation)
	}
	cleared := findCookie(recorder.Result().Cookies(), googleStateCookieName)
	if cleared == nil || cleared.MaxAge != -1 || cleared.Path != googleCallbackPath || cleared.SameSite != http.SameSiteLaxMode || !cleared.HttpOnly || !cleared.Secure {
		t.Fatalf("cleared state cookie = %+v", cleared)
	}
	if session := findCookie(recorder.Result().Cookies(), cookieName); session != nil && session.Value != "" {
		t.Fatalf("failure set session cookie: %+v", session)
	}
	visible := recorder.Header().Get("Location") + recorder.Body.String()
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(visible, secret) {
			t.Fatalf("failure leaked %q: %s", secret, visible)
		}
	}
}

func TestGoogleCallbackLinksExistingMemberAndSetsStrictSession(t *testing.T) {
	store, _ := googleAuthTestStore(t)
	ctx := context.Background()
	who, _, err := store.Register(ctx, "Google member", "member-http@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "http-subject", Email: "member-http@example.com"}}
	handler := newGoogleAuthHandler(store, provider)
	stateCookie := startGoogleAuth(t, handler, provider, "/", "192.0.2.20:1234")
	callback := googleCallback(handler, "state="+url.QueryEscape(stateCookie.Value)+"&code=fixture-code", "192.0.2.20:1234", stateCookie)
	if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != "/?googleAuth=link" {
		t.Fatalf("callback: %d location=%q body=%s", callback.Code, callback.Header().Get("Location"), callback.Body.String())
	}
	cleared := findCookie(callback.Result().Cookies(), googleStateCookieName)
	linkCookie := findCookie(callback.Result().Cookies(), googleLinkCookieName)
	if cleared == nil || cleared.MaxAge != -1 || linkCookie == nil || len(linkCookie.Value) != 43 || linkCookie.Path != googleLinkPath || !linkCookie.HttpOnly || !linkCookie.Secure || linkCookie.SameSite != http.SameSiteStrictMode || linkCookie.MaxAge != 600 {
		t.Fatalf("callback cookies: cleared=%+v link=%+v", cleared, linkCookie)
	}
	if session := findCookie(callback.Result().Cookies(), cookieName); session != nil && session.Value != "" {
		t.Fatalf("unlinked callback created session: %+v", session)
	}
	linked := completeGoogleLinkHTTP(handler, who.Email, "long-test-password-42", "192.0.2.21:1234", linkCookie)
	if linked.Code != http.StatusOK {
		t.Fatalf("link: %d %s", linked.Code, linked.Body.String())
	}
	session := findCookie(linked.Result().Cookies(), cookieName)
	clearedLink := findCookie(linked.Result().Cookies(), googleLinkCookieName)
	if session == nil || len(session.Value) != 43 || session.Path != "/" || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteStrictMode || clearedLink == nil || clearedLink.MaxAge != -1 {
		t.Fatalf("link cookies: session=%+v clearedLink=%+v", session, clearedLink)
	}
	me := googleAuthRequest(handler, http.MethodGet, "https://console.example.com/api/auth/me", "", "192.0.2.21:1234", session)
	if me.Code != http.StatusOK {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	var loaded platform.Identity
	if err = json.Unmarshal(me.Body.Bytes(), &loaded); err != nil || loaded.MemberID != who.MemberID {
		t.Fatalf("me identity = %+v, err = %v", loaded, err)
	}
	provider.mu.Lock()
	exchangeCode, calls := provider.exchangeCode, provider.exchangeCalls
	provider.mu.Unlock()
	if exchangeCode != "fixture-code" || calls != 1 {
		t.Fatalf("exchange code=%q calls=%d", exchangeCode, calls)
	}
}

func TestGoogleCallbackReturnsAdminToAdminWithoutGrantingPrivilege(t *testing.T) {
	store, _ := googleAuthTestStore(t)
	who, _, err := store.Register(context.Background(), "Admin visitor", "admin-http@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if who.PlatformAdmin {
		t.Fatal("fixture unexpectedly has platform access")
	}
	provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "admin-http-subject", Email: who.Email}}
	handler := newGoogleAuthHandler(store, provider)
	stateCookie := startGoogleAuth(t, handler, provider, "/admin", "192.0.2.30:1234")
	callback := googleCallback(handler, "state="+stateCookie.Value+"&code=admin-code", "192.0.2.30:1234", stateCookie)
	if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != "/admin?googleAuth=link" {
		t.Fatalf("admin callback: %d %q", callback.Code, callback.Header().Get("Location"))
	}
	linkCookie := findCookie(callback.Result().Cookies(), googleLinkCookieName)
	linked := completeGoogleLinkHTTP(handler, who.Email, "long-test-password-42", "192.0.2.31:1234", linkCookie)
	if linked.Code != http.StatusOK {
		t.Fatalf("admin link: %d %s", linked.Code, linked.Body.String())
	}
	session := findCookie(linked.Result().Cookies(), cookieName)
	me := googleAuthRequest(handler, http.MethodGet, "https://console.example.com/api/auth/me", "", "192.0.2.31:1234", session)
	var loaded platform.Identity
	if err = json.Unmarshal(me.Body.Bytes(), &loaded); err != nil || loaded.PlatformAdmin || loaded.MemberID != who.MemberID {
		t.Fatalf("admin return granted privilege: %+v, err = %v", loaded, err)
	}
}

func TestGoogleCallbackRejectsReplayExpiryStateAndNonce(t *testing.T) {
	t.Run("state mismatch does not consume attempt", func(t *testing.T) {
		store, _ := googleAuthTestStore(t)
		who, _, err := store.Register(context.Background(), "Mismatch", "mismatch@example.com", "long-test-password-42")
		if err != nil {
			t.Fatal(err)
		}
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "mismatch-subject", Email: who.Email}}
		handler := newGoogleAuthHandler(store, provider)
		stateCookie := startGoogleAuth(t, handler, provider, "/", "192.0.2.40:1234")
		mismatch := strings.Repeat("Z", 43)
		failed := googleCallback(handler, "state="+mismatch+"&code=state-secret-code", "192.0.2.40:1234", stateCookie)
		assertGoogleCallbackFailure(t, failed, "/", mismatch, "state-secret-code", stateCookie.Value)
		provider.mu.Lock()
		calls := provider.exchangeCalls
		provider.mu.Unlock()
		if calls != 0 {
			t.Fatalf("state mismatch exchanged code %d times", calls)
		}
		success := googleCallback(handler, "state="+stateCookie.Value+"&code=correct-code", "192.0.2.41:1234", stateCookie)
		if success.Code != http.StatusSeeOther || success.Header().Get("Location") != "/?googleAuth=link" || findCookie(success.Result().Cookies(), googleLinkCookieName) == nil {
			t.Fatalf("unconsumed attempt did not succeed: %d %s", success.Code, success.Body.String())
		}
	})

	t.Run("replay", func(t *testing.T) {
		store, _ := googleAuthTestStore(t)
		who, _, err := store.Register(context.Background(), "Replay", "replay@example.com", "long-test-password-42")
		if err != nil {
			t.Fatal(err)
		}
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "replay-subject", Email: who.Email}}
		handler := newGoogleAuthHandler(store, provider)
		stateCookie := startGoogleAuth(t, handler, provider, "/", "192.0.2.50:1234")
		query := "state=" + stateCookie.Value + "&code=replay-code"
		if first := googleCallback(handler, query, "192.0.2.50:1234", stateCookie); findCookie(first.Result().Cookies(), googleLinkCookieName) == nil {
			t.Fatalf("first callback failed: %d %s", first.Code, first.Body.String())
		}
		replay := googleCallback(handler, query, "192.0.2.51:1234", stateCookie)
		assertGoogleCallbackFailure(t, replay, "/", stateCookie.Value, "replay-code")
	})

	t.Run("expired", func(t *testing.T) {
		store, inspection := googleAuthTestStore(t)
		who, _, err := store.Register(context.Background(), "Expired", "expired@example.com", "long-test-password-42")
		if err != nil {
			t.Fatal(err)
		}
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "expired-subject", Email: who.Email}}
		handler := newGoogleAuthHandler(store, provider)
		stateCookie := startGoogleAuth(t, handler, provider, "/admin", "192.0.2.60:1234")
		hash := sha256.Sum256([]byte(stateCookie.Value))
		if _, err = inspection.Exec(context.Background(), `UPDATE oauth_login_attempts SET created_at=now()-interval '20 minutes',expires_at=now()-interval '10 minutes' WHERE state_hash=$1`, fmt.Sprintf("%x", hash[:])); err != nil {
			t.Fatal(err)
		}
		failed := googleCallback(handler, "state="+stateCookie.Value+"&code=expired-code", "192.0.2.60:1234", stateCookie)
		assertGoogleCallbackFailure(t, failed, "/", stateCookie.Value, "expired-code")
		provider.mu.Lock()
		calls := provider.exchangeCalls
		provider.mu.Unlock()
		if calls != 0 {
			t.Fatalf("expired attempt exchanged code %d times", calls)
		}
	})

	t.Run("provider nonce rejection", func(t *testing.T) {
		store, _ := googleAuthTestStore(t)
		who, _, err := store.Register(context.Background(), "Nonce", "nonce@example.com", "long-test-password-42")
		if err != nil {
			t.Fatal(err)
		}
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "nonce-subject", Email: who.Email}, exchangeErr: errors.New("provider nonce mismatch with secret-token")}
		handler := newGoogleAuthHandler(store, provider)
		stateCookie := startGoogleAuth(t, handler, provider, "/", "192.0.2.70:1234")
		failed := googleCallback(handler, "state="+stateCookie.Value+"&code=nonce-code", "192.0.2.70:1234", stateCookie)
		assertGoogleCallbackFailure(t, failed, "/", stateCookie.Value, "nonce-code", "secret-token")
	})
}

func TestGoogleCallbackRejectsUnknownUnverifiedAndSubjectConflict(t *testing.T) {
	t.Run("unknown email", func(t *testing.T) {
		store, inspection := googleAuthTestStore(t)
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "unknown-http-subject", Email: "unknown-http@example.com"}}
		handler := newGoogleAuthHandler(store, provider)
		stateCookie := startGoogleAuth(t, handler, provider, "/", "192.0.2.80:1234")
		pending := googleCallback(handler, "state="+stateCookie.Value+"&code=unknown-code", "192.0.2.80:1234", stateCookie)
		if pending.Code != http.StatusSeeOther || pending.Header().Get("Location") != "/?googleAuth=link" {
			t.Fatalf("unknown callback: %d %q", pending.Code, pending.Header().Get("Location"))
		}
		linkCookie := findCookie(pending.Result().Cookies(), googleLinkCookieName)
		failed := completeGoogleLinkHTTP(handler, "unknown-http@example.com", "long-test-password-42", "192.0.2.80:1234", linkCookie)
		if failed.Code != http.StatusUnauthorized || strings.Contains(failed.Body.String(), "unknown-http-subject") || strings.Contains(failed.Body.String(), stateCookie.Value) {
			t.Fatalf("unknown link response: %d %s", failed.Code, failed.Body.String())
		}
		var bindings, sessions int
		if err := inspection.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM member_external_identities),(SELECT count(*) FROM sessions)`).Scan(&bindings, &sessions); err != nil || bindings != 0 || sessions != 0 {
			t.Fatalf("unknown callback wrote bindings=%d sessions=%d err=%v", bindings, sessions, err)
		}
	})

	t.Run("provider rejects unverified email", func(t *testing.T) {
		store, _ := googleAuthTestStore(t)
		who, _, err := store.Register(context.Background(), "Unverified", "unverified-http@example.com", "long-test-password-42")
		if err != nil {
			t.Fatal(err)
		}
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "unverified-http-subject", Email: who.Email}, exchangeErr: errors.New("email_verified=false provider-secret")}
		handler := newGoogleAuthHandler(store, provider)
		stateCookie := startGoogleAuth(t, handler, provider, "/", "192.0.2.81:1234")
		failed := googleCallback(handler, "state="+stateCookie.Value+"&code=unverified-code", "192.0.2.81:1234", stateCookie)
		assertGoogleCallbackFailure(t, failed, "/", "unverified-code", "provider-secret")
	})

	t.Run("second subject for bound member", func(t *testing.T) {
		store, _ := googleAuthTestStore(t)
		who, _, err := store.Register(context.Background(), "Conflict", "conflict-http@example.com", "long-test-password-42")
		if err != nil {
			t.Fatal(err)
		}
		provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "first-http-subject", Email: who.Email}}
		handler := newGoogleAuthHandler(store, provider)
		firstState := startGoogleAuth(t, handler, provider, "/", "192.0.2.82:1234")
		first := googleCallback(handler, "state="+firstState.Value+"&code=first-code", "192.0.2.82:1234", firstState)
		firstLink := findCookie(first.Result().Cookies(), googleLinkCookieName)
		if firstLink == nil {
			t.Fatalf("first binding failed: %d %s", first.Code, first.Body.String())
		}
		if linked := completeGoogleLinkHTTP(handler, who.Email, "long-test-password-42", "192.0.2.82:1234", firstLink); linked.Code != http.StatusOK {
			t.Fatalf("first link failed: %d %s", linked.Code, linked.Body.String())
		}
		provider.mu.Lock()
		provider.claims.Subject = "second-http-subject"
		provider.mu.Unlock()
		secondState := startGoogleAuth(t, handler, provider, "/admin", "192.0.2.83:1234")
		pending := googleCallback(handler, "state="+secondState.Value+"&code=conflict-code", "192.0.2.83:1234", secondState)
		if pending.Code != http.StatusSeeOther || pending.Header().Get("Location") != "/admin?googleAuth=link" {
			t.Fatalf("conflicting subject callback: %d %q", pending.Code, pending.Header().Get("Location"))
		}
		failed := completeGoogleLinkHTTP(handler, who.Email, "long-test-password-42", "192.0.2.83:1234", findCookie(pending.Result().Cookies(), googleLinkCookieName))
		if failed.Code != http.StatusUnauthorized || strings.Contains(failed.Body.String(), "conflict-code") || strings.Contains(failed.Body.String(), secondState.Value) {
			t.Fatalf("conflicting link response: %d %s", failed.Code, failed.Body.String())
		}
	})
}

func TestGoogleCallbackErrorsAreBoundedAndSecretFree(t *testing.T) {
	store, _ := googleAuthTestStore(t)
	who, _, err := store.Register(context.Background(), "Provider error", "provider-error@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeGoogleProvider{claims: googleauth.Claims{Subject: "provider-error-subject", Email: who.Email}}
	handler := newGoogleAuthHandler(store, provider)
	stateCookie := startGoogleAuth(t, handler, provider, "/admin", "192.0.2.90:1234")
	provider.mu.Lock()
	nonce, verifier := provider.nonce, provider.verifier
	provider.mu.Unlock()
	denial := googleCallback(handler, "state="+stateCookie.Value+"&error=access_denied&error_description=provider-description-secret", "192.0.2.90:1234", stateCookie)
	assertGoogleCallbackFailure(t, denial, "/admin", stateCookie.Value, nonce, verifier, "access_denied", "provider-description-secret")
	provider.mu.Lock()
	calls := provider.exchangeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider denial exchanged code %d times", calls)
	}

	secondState := startGoogleAuth(t, handler, provider, "/", "192.0.2.91:1234")
	provider.mu.Lock()
	nonce, verifier = provider.nonce, provider.verifier
	provider.exchangeErr = fmt.Errorf("upstream token provider-secret code-secret %s %s", nonce, verifier)
	provider.mu.Unlock()
	failed := googleCallback(handler, "state="+secondState.Value+"&code=code-secret", "192.0.2.91:1234", secondState)
	assertGoogleCallbackFailure(t, failed, "/", secondState.Value, nonce, verifier, "provider-secret", "code-secret")
}
