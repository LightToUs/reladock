package sshdeploy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

const (
	remoteCommand  = "/usr/bin/sudo -n /bin/bash -seu"
	maxOutputBytes = 32 * 1024
)

var (
	ansiPattern   = regexp.MustCompile("\\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\\x07]*(?:\\x07|\\x1b\\\\))")
	secretPattern = regexp.MustCompile(`(?im)([a-z0-9_]*(?:password|token|secret|authorization)[a-z0-9_]*)[ \t]*[:=][ \t]*[^\r\n]*`)
)

type Request struct {
	Address        string
	Username       string
	PrivateKeyPEM  string
	Certificate    string
	HostPublicKeys []string
	Script         string
	ConnectTimeout time.Duration
	RunTimeout     time.Duration
}

type Result struct {
	Started       bool
	ExitCode      int
	Output        string
	HostKeySHA256 string
}

type Runner struct{}

func dialAddress(value string) (string, error) {
	if host, port, err := net.SplitHostPort(value); err == nil && host != "" && port != "" {
		return value, nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return "", errors.New("invalid SSH address")
	}
	return net.JoinHostPort(address.String(), "22"), nil
}

func (Runner) Run(ctx context.Context, request Request) (Result, error) {
	result := Result{ExitCode: -1}
	if request.Address == "" || request.Username == "" || request.PrivateKeyPEM == "" || len(request.HostPublicKeys) == 0 || request.Script == "" || request.ConnectTimeout <= 0 || request.RunTimeout <= 0 {
		return result, errors.New("invalid SSH deployment request")
	}
	signer, err := ssh.ParsePrivateKey([]byte(request.PrivateKeyPEM))
	if err != nil {
		return result, errors.New("invalid SSH private key")
	}
	if request.Certificate != "" {
		publicKey, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(request.Certificate))
		if parseErr != nil {
			return result, errors.New("invalid SSH certificate")
		}
		certificate, ok := publicKey.(*ssh.Certificate)
		if !ok {
			return result, errors.New("invalid SSH certificate")
		}
		signer, err = ssh.NewCertSigner(certificate, signer)
		if err != nil {
			return result, errors.New("SSH certificate does not match private key")
		}
	}

	witnessed := make(map[string]struct{}, len(request.HostPublicKeys))
	for _, encoded := range request.HostPublicKeys {
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(encoded))
		if parseErr != nil {
			return result, errors.New("invalid witnessed SSH host key")
		}
		witnessed[string(key.Marshal())] = struct{}{}
	}
	config := &ssh.ClientConfig{
		User: request.Username,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if _, ok := witnessed[string(key.Marshal())]; !ok {
				return errors.New("SSH host key was not witnessed by AWS")
			}
			result.HostKeySHA256 = ssh.FingerprintSHA256(key)
			return nil
		},
	}

	address, err := dialAddress(request.Address)
	if err != nil {
		return result, err
	}
	connectCtx, cancelConnect := context.WithTimeout(ctx, request.ConnectTimeout)
	defer cancelConnect()
	connection, err := (&net.Dialer{}).DialContext(connectCtx, "tcp", address)
	if err != nil {
		return result, contextOrSanitized(connectCtx, "SSH connection failed")
	}
	defer connection.Close()
	if deadline, ok := connectCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-connectCtx.Done():
			_ = connection.Close()
		case <-handshakeDone:
		}
	}()
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, request.Address, config)
	close(handshakeDone)
	if err != nil {
		return result, contextOrSanitized(connectCtx, "SSH handshake failed")
	}
	_ = connection.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConnection, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return result, errors.New("SSH session failed")
	}
	defer session.Close()

	output := &tailBuffer{maximum: maxOutputBytes}
	session.Stdout = output
	session.Stderr = output
	session.Stdin = strings.NewReader(request.Script)
	if err := session.Start(remoteCommand); err != nil {
		return result, errors.New("SSH command did not start")
	}
	result.Started = true
	runCtx, cancelRun := context.WithTimeout(ctx, request.RunTimeout)
	defer cancelRun()
	wait := make(chan error, 1)
	go func() { wait <- session.Wait() }()
	select {
	case waitErr := <-wait:
		result.Output = sanitizeOutput(output.Bytes())
		if waitErr == nil {
			result.ExitCode = 0
			return result, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitStatus()
			return result, nil
		}
		return result, errors.New("SSH command result is unknown")
	case <-runCtx.Done():
		_ = session.Close()
		_ = client.Close()
		result.Output = sanitizeOutput(output.Bytes())
		return result, runCtx.Err()
	}
}

func contextOrSanitized(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New(message)
}

type tailBuffer struct {
	mu      sync.Mutex
	data    []byte
	maximum int
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	if len(data) >= b.maximum {
		b.data = append(b.data[:0], data[len(data)-b.maximum:]...)
		return written, nil
	}
	if overflow := len(b.data) + len(data) - b.maximum; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, data...)
	return written, nil
}

func (b *tailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data)
}

func sanitizeOutput(raw []byte) string {
	if !utf8.Valid(raw) {
		raw = bytes.ToValidUTF8(raw, []byte("?"))
	}
	text := ansiPattern.ReplaceAllString(string(raw), "")
	text = secretPattern.ReplaceAllString(text, "${1}=[REDACTED]")
	text = strings.Map(func(value rune) rune {
		if value == '\n' || value == '\r' || value == '\t' || value >= 0x20 && value != 0x7f {
			return value
		}
		return -1
	}, text)
	if len(text) > maxOutputBytes {
		text = text[len(text)-maxOutputBytes:]
		if !utf8.ValidString(text) {
			text = string(bytes.ToValidUTF8([]byte(text), []byte("?")))
		}
	}
	return text
}

var _ io.Writer = (*tailBuffer)(nil)
