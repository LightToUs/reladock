package googleauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const maxProviderResponseBytes int64 = 1 << 20

var errGoogleAuthentication = errors.New("Google authentication failed")

type Claims struct {
	Subject string
	Email   string
}

type Provider interface {
	AuthCodeURL(state, nonce, verifier string) string
	Exchange(ctx context.Context, code, verifier, nonce string) (Claims, error)
}

type providerEndpoints struct {
	Issuer           string
	AuthorizationURL string
	TokenURL         string
	JWKSURL          string
}

type provider struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

func New(config Config) (Provider, error) {
	return newProvider(config, providerEndpoints{
		Issuer:           "https://accounts.google.com",
		AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:         "https://oauth2.googleapis.com/token",
		JWKSURL:          "https://www.googleapis.com/oauth2/v3/certs",
	})
}

func newProvider(config Config, endpoints providerEndpoints) (Provider, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if endpoints.Issuer == "" || endpoints.AuthorizationURL == "" || endpoints.TokenURL == "" || endpoints.JWKSURL == "" {
		return nil, errors.New("Google OIDC endpoints are invalid")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport:     boundedTransport{base: transport, limit: maxProviderResponseBytes},
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx := oidc.ClientContext(context.Background(), client)
	oidcProvider := (&oidc.ProviderConfig{
		IssuerURL:  endpoints.Issuer,
		AuthURL:    endpoints.AuthorizationURL,
		TokenURL:   endpoints.TokenURL,
		JWKSURL:    endpoints.JWKSURL,
		Algorithms: []string{"RS256"},
	}).NewProvider(ctx)
	return &provider{
		oauth: oauth2.Config{
			ClientID: config.ClientID, ClientSecret: config.ClientSecret,
			RedirectURL: config.RedirectURL,
			Endpoint:    oauth2.Endpoint{AuthURL: endpoints.AuthorizationURL, TokenURL: endpoints.TokenURL},
			Scopes:      []string{oidc.ScopeOpenID, "email"},
		},
		verifier: oidcProvider.VerifierContext(ctx, &oidc.Config{
			ClientID: config.ClientID, SupportedSigningAlgs: []string{"RS256"},
		}),
		client: client,
	}, nil
}

func (p *provider) AuthCodeURL(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.S256ChallengeOption(verifier),
	)
}

func (p *provider) Exchange(ctx context.Context, code, verifier, nonce string) (Claims, error) {
	if !validProtocolValue(code, 4096) || !validProtocolValue(verifier, 256) || !validProtocolValue(nonce, 256) {
		return Claims{}, errGoogleAuthentication
	}
	ctx = oidc.ClientContext(ctx, p.client)
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Claims{}, errGoogleAuthentication
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || !validProtocolValue(rawIDToken, int(maxProviderResponseBytes)) {
		return Claims{}, errGoogleAuthentication
	}
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Claims{}, errGoogleAuthentication
	}
	var rawClaims struct {
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Nonce         string `json:"nonce"`
	}
	if err := idToken.Claims(&rawClaims); err != nil || !rawClaims.EmailVerified ||
		!validProtocolValue(rawClaims.Subject, 255) || !validProtocolValue(rawClaims.Email, 320) ||
		subtle.ConstantTimeCompare([]byte(rawClaims.Nonce), []byte(nonce)) != 1 {
		return Claims{}, errGoogleAuthentication
	}
	return Claims{Subject: rawClaims.Subject, Email: rawClaims.Email}, nil
}

func validProtocolValue(value string, max int) bool {
	return value != "" && len(value) <= max && strings.IndexFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) < 0
}

type boundedTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > t.limit {
		response.Body.Close()
		return nil, errors.New("provider response too large")
	}
	response.Body = &boundedReadCloser{body: response.Body, remaining: t.limit}
	return response, nil
}

type boundedReadCloser struct {
	body      io.ReadCloser
	remaining int64
}

func (r *boundedReadCloser) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.body.Read(extra[:])
		if n > 0 {
			return 0, errors.New("provider response too large")
		}
		return 0, err
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	n, err := r.body.Read(buffer)
	r.remaining -= int64(n)
	return n, err
}

func (r *boundedReadCloser) Close() error { return r.body.Close() }
