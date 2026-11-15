package sshdeploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type testSSHServer struct {
	address        string
	hostPublicKey  string
	privateKeyPEM  string
	certificate    string
	command        chan string
	stdin          chan string
	connections    atomic.Int32
	exitCode       uint32
	output         string
	waitForContext bool
	close          func()
}

func TestDialAddressDefaultsStaticIPsToSSHPort(t *testing.T) {
	for input, want := range map[string]string{
		"203.0.113.18":   "203.0.113.18:22",
		"2001:db8::18":   "[2001:db8::18]:22",
		"127.0.0.1:2222": "127.0.0.1:2222",
		"[::1]:2222":     "[::1]:2222",
	} {
		got, err := dialAddress(input)
		if err != nil || got != want {
			t.Fatalf("dialAddress(%q)=%q,%v want %q", input, got, err, want)
		}
	}
	if _, err := dialAddress("not-an-ip"); err == nil {
		t.Fatal("accepted non-IP SSH target")
	}
}

func newTestSSHServer(t *testing.T, output string, exitCode uint32, waitForContext bool) *testSSHServer {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	_, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caSigner, err := ssh.NewSignerFromKey(caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &ssh.Certificate{Key: clientSigner.PublicKey(), CertType: ssh.UserCert, ValidPrincipals: []string{"admin"}, ValidAfter: 0, ValidBefore: ssh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testSSHServer{
		address: listener.Addr().String(), hostPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))),
		privateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})), certificate: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(certificate))),
		command: make(chan string, 2), stdin: make(chan string, 2), exitCode: exitCode, output: output, waitForContext: waitForContext,
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		cert, ok := key.(*ssh.Certificate)
		if !ok || string(cert.Key.Marshal()) != string(clientPublicKey(t, clientPublic).Marshal()) {
			return nil, errors.New("certificate required")
		}
		return nil, nil
	}}
	config.AddHostKey(hostSigner)
	ctx, cancel := context.WithCancel(context.Background())
	server.close = func() { cancel(); _ = listener.Close() }
	t.Cleanup(server.close)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			server.connections.Add(1)
			go serveTestSSHConnection(ctx, connection, config, server)
		}
	}()
	return server
}

func clientPublicKey(t *testing.T, key ed25519.PublicKey) ssh.PublicKey {
	t.Helper()
	publicKey, err := ssh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

func serveTestSSHConnection(ctx context.Context, connection net.Conn, config *ssh.ServerConfig, server *testSSHServer) {
	defer connection.Close()
	_, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for channelRequest := range channels {
		if channelRequest.ChannelType() != "session" {
			_ = channelRequest.Reject(ssh.UnknownChannelType, "session required")
			continue
		}
		channel, requests, err := channelRequest.Accept()
		if err != nil {
			return
		}
		go func() {
			defer channel.Close()
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if ssh.Unmarshal(request.Payload, &payload) != nil {
					_ = request.Reply(false, nil)
					continue
				}
				server.command <- payload.Command
				_ = request.Reply(true, nil)
				body, _ := io.ReadAll(channel)
				server.stdin <- string(body)
				if server.waitForContext {
					<-ctx.Done()
					return
				}
				_, _ = io.WriteString(channel, server.output)
				status := struct{ Status uint32 }{Status: server.exitCode}
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(&status))
				return
			}
		}()
	}
}

func (s *testSSHServer) request(script string) Request {
	return Request{
		Address: s.address, Username: "admin", PrivateKeyPEM: s.privateKeyPEM, Certificate: s.certificate,
		HostPublicKeys: []string{s.hostPublicKey}, Script: script, ConnectTimeout: time.Second, RunTimeout: time.Second,
	}
}

func TestRunnerUsesWitnessedHostCertificateAndFixedCommandOnce(t *testing.T) {
	server := newTestSSHServer(t, "deployed\n", 0, false)
	result, err := (Runner{}).Run(context.Background(), server.request("echo deploy\n"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Started || result.ExitCode != 0 || result.Output != "deployed\n" || !strings.HasPrefix(result.HostKeySHA256, "SHA256:") {
		t.Fatalf("result: %+v", result)
	}
	if command := <-server.command; command != "/usr/bin/sudo -n /bin/bash -seu" {
		t.Fatalf("command = %q", command)
	}
	if body := <-server.stdin; body != "echo deploy\n" {
		t.Fatalf("stdin = %q", body)
	}
	select {
	case duplicate := <-server.stdin:
		t.Fatalf("script sent twice: %q", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRunnerRejectsUnwitnessedHostBeforeExecution(t *testing.T) {
	server := newTestSSHServer(t, "", 0, false)
	request := server.request("echo forbidden\n")
	_, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherSigner, err := ssh.NewSignerFromKey(otherPrivate)
	if err != nil {
		t.Fatal(err)
	}
	request.HostPublicKeys = []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(otherSigner.PublicKey())))}
	result, err := (Runner{}).Run(context.Background(), request)
	if err == nil || result.Started {
		t.Fatalf("unwitnessed host accepted: %+v %v", result, err)
	}
	select {
	case command := <-server.command:
		t.Fatalf("command executed after host mismatch: %q", command)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRunnerCapturesNonZeroAndBoundsSanitizesOutput(t *testing.T) {
	unsafe := strings.Repeat("x", 40*1024) + "\nprefix\x1b[31mred\x1b[0m\nPASSWORD=should-not-escape\nauthorization: Bearer hidden\n"
	server := newTestSSHServer(t, unsafe, 23, false)
	result, err := (Runner{}).Run(context.Background(), server.request("exit 23\n"))
	if err != nil {
		t.Fatalf("non-zero exit returned transport error: %v", err)
	}
	if !result.Started || result.ExitCode != 23 || len(result.Output) > 32*1024 || strings.Contains(result.Output, "\x1b") || strings.Contains(result.Output, "should-not-escape") || strings.Contains(result.Output, "Bearer hidden") {
		t.Fatalf("unsafe result: started=%t exit=%d bytes=%d output=%q", result.Started, result.ExitCode, len(result.Output), result.Output[:min(100, len(result.Output))])
	}
}

func TestRunnerRunTimeoutIsUnknownAfterStart(t *testing.T) {
	server := newTestSSHServer(t, "", 0, true)
	request := server.request("sleep forever\n")
	request.RunTimeout = 30 * time.Millisecond
	result, err := (Runner{}).Run(context.Background(), request)
	if !errors.Is(err, context.DeadlineExceeded) || !result.Started {
		t.Fatalf("timeout result: %+v %v", result, err)
	}
}
