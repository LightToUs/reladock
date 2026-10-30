// Package runtimeconfig validates deployment configuration before opening a database or listener.
package runtimeconfig

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Config struct {
	Production                         bool
	AllowUnentitledRules               bool
	Address, Origin, CertFile, KeyFile string
	AgentAddress, AgentTLSDir          string
	Bootstrap                          BootstrapConfig
}

func Load(get func(string) string) (Config, error) {
	c := Config{Address: get("LISTEN_ADDRESS"), Origin: get("PUBLIC_ORIGIN"), CertFile: get("TLS_CERT_FILE"), KeyFile: get("TLS_KEY_FILE"), AgentAddress: get("AGENT_LISTEN_ADDRESS"), AgentTLSDir: get("AGENT_TLS_DIR")}
	allowUnentitled, err := LoadDevelopmentEntitlementBypass(get)
	if err != nil {
		return c, err
	}
	c.AllowUnentitledRules = allowUnentitled
	mode := get("APP_ENV")
	if mode != "" && mode != "development" && mode != "production" {
		return c, errors.New("APP_ENV must be development or production")
	}
	c.Production = mode == "production"
	port := get("PORT")
	if port == "" {
		port = "3001"
	}
	if !validPort(port) {
		return c, errors.New("invalid PORT")
	}
	if c.Address == "" {
		c.Address = "127.0.0.1:" + port
	}
	if !validAddress(c.Address) {
		return c, errors.New("LISTEN_ADDRESS requires a literal IP and valid port")
	}
	agentPort := get("AGENT_PORT")
	if agentPort == "" {
		agentPort = "3002"
	}
	if !validPort(agentPort) {
		return c, errors.New("invalid AGENT_PORT")
	}
	if c.AgentAddress == "" {
		c.AgentAddress = "127.0.0.1:" + agentPort
	}
	if !validAddress(c.AgentAddress) || c.AgentTLSDir != "" && c.AgentAddress == c.Address {
		return c, errors.New("invalid or conflicting AGENT_LISTEN_ADDRESS")
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return c, errors.New("TLS_CERT_FILE and TLS_KEY_FILE are required together")
	}
	if c.Production && (c.Origin == "" || c.CertFile == "") {
		return c, errors.New("production requires PUBLIC_ORIGIN and HTTPS certificates")
	}
	if !loopback(c.Address) && c.CertFile == "" {
		return c, errors.New("non-loopback HTTP is prohibited; configure TLS")
	}
	if !loopback(c.AgentAddress) && c.AgentTLSDir == "" {
		return c, errors.New("public Agent listener requires AGENT_TLS_DIR")
	}
	if c.Origin == "" {
		if !loopback(c.Address) {
			return c, errors.New("public binding requires explicit PUBLIC_ORIGIN")
		}
		scheme := "http://"
		if c.CertFile != "" {
			scheme = "https://"
		}
		c.Origin = scheme + c.Address
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(c.Origin, "\r\n\t ") || (u.Scheme != "https" && u.Scheme != "http") {
		return c, errors.New("PUBLIC_ORIGIN must be an exact HTTP(S) origin without path, credentials, query or fragment")
	}
	if u.Port() != "" && !validPort(u.Port()) {
		return c, errors.New("invalid PUBLIC_ORIGIN port")
	}
	if (c.CertFile != "") != (u.Scheme == "https") {
		return c, errors.New("PUBLIC_ORIGIN scheme must match configured TLS")
	}
	if c.Production && u.Scheme != "https" {
		return c, errors.New("production requires HTTPS")
	}
	bootstrap, err := loadBootstrap(get, c)
	if err != nil {
		return c, err
	}
	c.Bootstrap = bootstrap
	return c, nil
}

func LoadDevelopmentEntitlementBypass(get func(string) string) (bool, error) {
	switch get("RELADOCK_DEVELOPMENT_ALLOW_UNENTITLED_RULES") {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("RELADOCK_DEVELOPMENT_ALLOW_UNENTITLED_RULES must be true or false")
	}
}

func validPort(s string) bool {
	n, e := strconv.Atoi(s)
	return e == nil && n > 0 && n < 65536 && strconv.Itoa(n) == s
}
func validAddress(s string) bool {
	h, p, e := net.SplitHostPort(s)
	return e == nil && net.ParseIP(h) != nil && validPort(p)
}
func loopback(s string) bool { h, _, _ := net.SplitHostPort(s); return net.ParseIP(h).IsLoopback() }
