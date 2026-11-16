package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"time"

	"reladock/internal/platform"
)

const (
	googleStateCookieName = "reladock_google_state"
	googleCallbackPath    = "/api/auth/google/callback"
)

func (s *server) serveGoogleAuth(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == "/api/auth/google/status":
		if r.Method != http.MethodGet {
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不支持"})
			return true
		}
		respond(w, http.StatusOK, map[string]bool{"enabled": s.googleAuth != nil})
		return true
	case r.URL.Path == "/api/auth/google/start":
		if r.Method != http.MethodPost {
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不支持"})
			return true
		}
		if s.googleAuth == nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Google 登录未启用"})
			return true
		}
		var input struct {
			ReturnTo string `json:"returnTo"`
		}
		if !decode(w, r, &input) {
			return true
		}
		if input.ReturnTo != "/" && input.ReturnTo != "/admin" {
			failure(w, platform.ErrInvalid)
			return true
		}
		state, err := randomGoogleToken()
		if err != nil {
			failure(w, err)
			return true
		}
		nonce, err := randomGoogleToken()
		if err != nil {
			failure(w, err)
			return true
		}
		verifier, err := randomGoogleToken()
		if err != nil {
			failure(w, err)
			return true
		}
		if err = s.store.StartGoogleLogin(r.Context(), state, verifier, nonce, input.ReturnTo); err != nil {
			failure(w, err)
			return true
		}
		s.googleStateCookie(w, state)
		respond(w, http.StatusOK, map[string]string{"authorizationURL": s.googleAuth.AuthCodeURL(state, nonce, verifier)})
		return true
	case r.URL.Path == googleCallbackPath:
		s.googleStateCookie(w, "")
		if r.Method != http.MethodGet {
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不支持"})
			return true
		}
		s.serveGoogleCallback(w, r)
		return true
	}
	return false
}

func (s *server) serveGoogleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(returnTo string) {
		if returnTo != "/admin" {
			returnTo = "/"
		}
		http.Redirect(w, r, returnTo+"?googleAuth=failed", http.StatusSeeOther)
	}
	query := r.URL.Query()
	stateValues := query["state"]
	cookie, cookieErr := r.Cookie(googleStateCookieName)
	if cookieErr != nil || len(stateValues) != 1 || len(cookie.Value) != 43 || len(stateValues[0]) != 43 ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(stateValues[0])) != 1 {
		fail("/")
		return
	}
	attempt, err := s.store.ConsumeGoogleLogin(r.Context(), cookie.Value)
	if err != nil {
		fail("/")
		return
	}
	returnTo := attempt.ReturnTo
	if s.googleAuth == nil || query.Get("error") != "" {
		fail(returnTo)
		return
	}
	codeValues := query["code"]
	if len(codeValues) != 1 || codeValues[0] == "" || len(codeValues[0]) > 4096 {
		fail(returnTo)
		return
	}
	claims, err := s.googleAuth.Exchange(r.Context(), codeValues[0], attempt.Verifier, attempt.Nonce)
	if err != nil {
		fail(returnTo)
		return
	}
	_, token, err := s.store.CompleteGoogleLogin(r.Context(), platform.GoogleIdentity{
		Subject: claims.Subject, Email: claims.Email, EmailVerified: true,
	})
	if err != nil {
		fail(returnTo)
		return
	}
	s.sessionCookie(w, token)
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func randomGoogleToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *server) googleStateCookie(w http.ResponseWriter, state string) {
	cookie := &http.Cookie{
		Name: googleStateCookieName, Value: state, Path: googleCallbackPath,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
		MaxAge: 600, Expires: time.Now().Add(10 * time.Minute),
	}
	if state == "" {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0)
	}
	http.SetCookie(w, cookie)
}
