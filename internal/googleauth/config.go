// Package googleauth implements the optional Google OpenID Connect login adapter.
package googleauth

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const callbackPath = "/api/auth/google/callback"

var googleClientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+\.apps\.googleusercontent\.com$`)

type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

func (c Config) Enabled() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

func LoadConfig(get func(string) string, origin string) (Config, error) {
	c := Config{
		ClientID:     get("GOOGLE_OIDC_CLIENT_ID"),
		ClientSecret: get("GOOGLE_OIDC_CLIENT_SECRET"),
	}
	if c.ClientID == "" && c.ClientSecret == "" {
		return Config{}, nil
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return Config{}, errors.New("GOOGLE_OIDC_CLIENT_ID and GOOGLE_OIDC_CLIENT_SECRET are required together")
	}
	if err := validateClientCredentials(c.ClientID, c.ClientSecret); err != nil {
		return Config{}, err
	}
	if !exactOrigin(origin) {
		return Config{}, errors.New("Google OIDC requires an exact PUBLIC_ORIGIN")
	}
	c.RedirectURL = origin + callbackPath
	return c, nil
}

func validateClientCredentials(clientID, secret string) error {
	if len(clientID) > 255 || !googleClientIDPattern.MatchString(clientID) {
		return errors.New("GOOGLE_OIDC_CLIENT_ID must be a Google web client ID")
	}
	if len(secret) == 0 || len(secret) > 512 || strings.IndexFunc(secret, func(r rune) bool {
		return r <= ' ' || r == 0x7f
	}) >= 0 {
		return errors.New("GOOGLE_OIDC_CLIENT_SECRET is invalid")
	}
	return nil
}

func exactOrigin(origin string) bool {
	if origin == "" || strings.ContainsAny(origin, "\r\n\t ") {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.Path == "" && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && u.Opaque == ""
}

func validateConfig(c Config) error {
	if !c.Enabled() {
		return errors.New("Google OIDC is not fully configured")
	}
	if err := validateClientCredentials(c.ClientID, c.ClientSecret); err != nil {
		return err
	}
	u, err := url.Parse(c.RedirectURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != callbackPath {
		return errors.New("Google OIDC redirect URL is invalid")
	}
	return nil
}
