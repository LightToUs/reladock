package retirement

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type scriptedRunner struct {
	calls     []Invocation
	responses []CommandResult
	errors    []error
}

func (r *scriptedRunner) Run(_ context.Context, call Invocation) (CommandResult, error) {
	r.calls = append(r.calls, call)
	i := len(r.calls) - 1
	var err error
	if i < len(r.errors) {
		err = r.errors[i]
	}
	if i < len(r.responses) {
		return r.responses[i], err
	}
	return CommandResult{}, err
}

func allocation(protocol string) Allocation {
	return Allocation{Address: "192.0.2.10", Protocol: protocol, Port: 20001}
}

func versionResult() CommandResult {
	return CommandResult{Stdout: []byte("conntrack v1.4.7 (conntrack-tools)\n")}
}

func zeroXML() []byte { return []byte(`<?xml version="1.0"?><conntrack></conntrack>`) }

func flowXML() []byte {
	return []byte(`<?xml version="1.0"?><conntrack><flow><meta direction="original"><layer3 protoname="ipv4"><dst>192.0.2.10</dst></layer3><layer4 protoname="tcp"><dport>20001</dport></layer4></meta></flow></conntrack>`)
}

func TestPurgeUsesOnlyFixedExactConntrackInvocations(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			runner := &scriptedRunner{responses: []CommandResult{
				versionResult(),
				{Stdout: []byte("2 flow entries have been deleted.\n")},
				{Stdout: zeroXML(), Stderr: []byte("conntrack v1.4.7 (conntrack-tools): 0 flow entries have been shown.\n")},
			}}
			result, err := (Executor{Runner: runner}).Purge(context.Background(), allocation(protocol))
			if err != nil || result.DeletedEntries != 2 || result.ConntrackVersion != "conntrack v1.4.7 (conntrack-tools)" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			want := []Invocation{
				{Path: ConntrackPath, Args: []string{"--version"}, Env: fixedEnvironment},
				{Path: ConntrackPath, Args: []string{"-D", "-f", "ipv4", "-p", protocol, "--orig-dst", "192.0.2.10", "--dport", "20001", "--zone", "0"}, Env: fixedEnvironment},
				{Path: ConntrackPath, Args: []string{"-L", "-f", "ipv4", "-p", protocol, "--orig-dst", "192.0.2.10", "--dport", "20001", "--zone", "0", "-o", "xml"}, Env: fixedEnvironment},
			}
			if !reflect.DeepEqual(runner.calls, want) {
				t.Fatalf("unsafe argv: %#v", runner.calls)
			}
			for _, call := range runner.calls {
				for _, arg := range call.Args {
					if arg == "-F" || arg == "--flush" || strings.Contains(arg, "sh -c") {
						t.Fatal("broad or shell operation", call)
					}
				}
			}
		})
	}
}

func TestPurgeIsIdempotentAndWaitsForStrictZero(t *testing.T) {
	runner := &scriptedRunner{responses: []CommandResult{
		versionResult(),
		{Stderr: []byte("conntrack v1.4.7 (conntrack-tools): 0 flow entries have been deleted.\n"), ExitCode: 1},
		{Stdout: flowXML(), Stderr: []byte("conntrack v1.4.7 (conntrack-tools): 1 flow entries have been shown.\n")},
		{Stdout: zeroXML(), Stderr: []byte("conntrack v1.4.7 (conntrack-tools): 0 flow entries have been shown.\n")},
	}}
	result, err := (Executor{Runner: runner, QueryAttempts: 3}).Purge(context.Background(), allocation("tcp"))
	if err != nil || result.DeletedEntries != 0 || len(runner.calls) != 4 {
		t.Fatalf("result=%+v calls=%d err=%v", result, len(runner.calls), err)
	}
}

func TestPurgeAcceptsRealDeleteOutputSplitAcrossStreams(t *testing.T) {
	runner := &scriptedRunner{responses: []CommandResult{
		versionResult(),
		{
			Stdout: []byte("tcp      6 431999 ESTABLISHED src=198.51.100.20 dst=192.0.2.10 sport=54321 dport=20001 [ASSURED] zone=0\n"),
			Stderr: []byte("conntrack v1.4.7 (conntrack-tools): 1 flow entries have been deleted.\n"),
		},
		{Stdout: zeroXML(), Stderr: []byte("conntrack v1.4.7 (conntrack-tools): 0 flow entries have been shown.\n")},
	}}
	result, err := (Executor{Runner: runner}).Purge(context.Background(), allocation("tcp"))
	if err != nil || result.DeletedEntries != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestDirectRunnerRejectsEveryCallerConstructedDangerousInvocation(t *testing.T) {
	runner := DirectRunner{}
	for name, args := range map[string][]string{
		"flush":             {"-F"},
		"unfiltered delete": {"-D"},
		"wrong address":     {"-D", "-f", "ipv4", "-p", "tcp", "--orig-dst", "0.0.0.0", "--dport", "20001", "--zone", "0"},
		"extra option":      {"-L", "-f", "ipv4", "-p", "tcp", "--orig-dst", "192.0.2.10", "--dport", "20001", "--zone", "0", "-o", "xml", "--dump"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runner.Run(context.Background(), Invocation{Path: ConntrackPath, Args: args, Env: fixedEnvironment})
			if err == nil || !strings.Contains(err.Error(), "invalid fixed conntrack invocation") {
				t.Fatalf("dangerous direct invocation accepted: %v", args)
			}
		})
	}
}

func TestPurgeRejectsInjectionUnsupportedToolAndUnboundedOutput(t *testing.T) {
	badAllocations := []Allocation{
		{Address: "192.0.2.10; --flush", Protocol: "tcp", Port: 20001},
		{Address: "2001:db8::1", Protocol: "tcp", Port: 20001},
		{Address: "0.0.0.0", Protocol: "tcp", Port: 20001},
		{Address: "192.0.2.10", Protocol: "both", Port: 20001},
		{Address: "192.0.2.10", Protocol: "tcp", Port: 1},
	}
	for _, input := range badAllocations {
		runner := &scriptedRunner{}
		if _, err := (Executor{Runner: runner}).Purge(context.Background(), input); ErrorCode(err) != "state-mismatch" || len(runner.calls) != 0 {
			t.Fatalf("unsafe allocation accepted: %+v %v", input, err)
		}
	}
	for name, version := range map[string]string{
		"old":       "conntrack v1.4.4 (conntrack-tools)\n",
		"future":    "conntrack v2.0.0 (conntrack-tools)\n",
		"malformed": "conntrack latest\n",
	} {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedRunner{responses: []CommandResult{{Stdout: []byte(version)}}}
			if _, err := (Executor{Runner: runner}).Purge(context.Background(), allocation("tcp")); ErrorCode(err) != "tool-version" || len(runner.calls) != 1 {
				t.Fatalf("version accepted: %v", err)
			}
		})
	}
	runner := &scriptedRunner{responses: []CommandResult{{Stdout: []byte(strings.Repeat("x", MaxCommandOutput+1))}}}
	if _, err := (Executor{Runner: runner}).Purge(context.Background(), allocation("tcp")); ErrorCode(err) != "output-invalid" {
		t.Fatalf("unbounded output accepted: %v", err)
	}
}

func TestPurgeRejectsMalformedTrailingAndPersistentXML(t *testing.T) {
	for name, body := range map[string][]byte{
		"malformed":       []byte(`<conntrack><flow></conntrack>`),
		"trailing":        []byte(`<conntrack></conntrack><conntrack></conntrack>`),
		"wrong root":      []byte(`<flows></flows>`),
		"unknown child":   []byte(`<conntrack><other/></conntrack>`),
		"unrelated tuple": []byte(`<?xml version="1.0"?><conntrack><flow><meta direction="original"><layer3 protoname="ipv4"><dst>192.0.2.99</dst></layer3><layer4 protoname="tcp"><dport>20001</dport></layer4></meta></flow></conntrack>`),
	} {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedRunner{responses: []CommandResult{versionResult(), {Stdout: []byte("1 flow entries have been deleted.\n")}, {Stdout: body}}}
			if _, err := (Executor{Runner: runner}).Purge(context.Background(), allocation("tcp")); ErrorCode(err) != "output-invalid" {
				t.Fatalf("bad XML accepted: %v", err)
			}
		})
	}
	runner := &scriptedRunner{responses: []CommandResult{versionResult(), {Stdout: []byte("1 flow entries have been deleted.\n")}, {Stdout: flowXML()}, {Stdout: flowXML()}}}
	if _, err := (Executor{Runner: runner, QueryAttempts: 2}).Purge(context.Background(), allocation("tcp")); ErrorCode(err) != "query-nonzero" {
		t.Fatalf("persistent entries accepted: %v", err)
	}
}

func TestPurgeMapsPermissionSyntaxAndIOWithoutLeakingRawOutput(t *testing.T) {
	cases := []struct {
		name      string
		responses []CommandResult
		errors    []error
		want      string
	}{
		{"missing", nil, []error{errors.New("executable not found")}, "tool-missing"},
		{"permission", []CommandResult{versionResult(), {ExitCode: 1, Stderr: []byte("conntrack: Operation not permitted\n")}}, nil, "permission-denied"},
		{"syntax", []CommandResult{versionResult(), {ExitCode: 1, Stderr: []byte("conntrack: unrecognized option '--zone'\n")}}, nil, "tool-version"},
		{"query", []CommandResult{versionResult(), {Stdout: []byte("1 flow entries have been deleted.\n")}, {ExitCode: 1, Stderr: []byte("query exploded secret=do-not-leak\n")}}, nil, "query-nonzero"},
		{"io", []CommandResult{versionResult()}, []error{nil, context.DeadlineExceeded}, "io-failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{responses: tc.responses, errors: tc.errors}
			_, err := (Executor{Runner: runner, Timeout: time.Millisecond}).Purge(context.Background(), allocation("tcp"))
			if ErrorCode(err) != tc.want || strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("code=%s err=%v", ErrorCode(err), err)
			}
		})
	}
}
