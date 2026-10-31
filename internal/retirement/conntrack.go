// Package retirement provides the fixed, bounded conntrack operation used by
// the Agent retirement state machine. It never invokes a shell or accepts raw
// command arguments from the controller.
package retirement

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/netip"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const ConntrackPath = "/usr/sbin/conntrack"
const MaxCommandOutput = 256 * 1024

var fixedEnvironment = []string{"LC_ALL=C", "LANG=C"}

type Invocation struct {
	Path string
	Args []string
	Env  []string
}

type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Runner interface {
	Run(context.Context, Invocation) (CommandResult, error)
}

type Allocation struct {
	Address  string
	Protocol string
	Port     int
}

type PurgeResult struct {
	ConntrackVersion string
	DeletedEntries   int
}

type Executor struct {
	Runner        Runner
	Timeout       time.Duration
	QueryAttempts int
}

type operationError struct{ code string }

func (e operationError) Error() string { return "conntrack retirement failed: " + e.code }

func ErrorCode(err error) string {
	var target operationError
	if errors.As(err, &target) {
		return target.code
	}
	return ""
}

func fail(code string) error { return operationError{code: code} }

var versionPattern = regexp.MustCompile(`^conntrack v1\.4\.([0-9]+) \(conntrack-tools\)\n?$`)
var deletePattern = regexp.MustCompile(`^(?:conntrack v1\.4\.[0-9]+ \(conntrack-tools\): )?([0-9]+) flow entries have been deleted\.\n?$`)
var shownPattern = regexp.MustCompile(`^conntrack v1\.4\.[0-9]+ \(conntrack-tools\): ([0-9]+) flow entries have been shown\.\n?$`)

func normalizeAllocation(input Allocation) (Allocation, error) {
	address, err := netip.ParseAddr(input.Address)
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsLoopback() || address.String() != input.Address || (input.Protocol != "tcp" && input.Protocol != "udp") || input.Port < 20000 || input.Port > 60000 {
		return Allocation{}, fail("state-mismatch")
	}
	return Allocation{Address: address.String(), Protocol: input.Protocol, Port: input.Port}, nil
}

func (e Executor) settings() (time.Duration, int) {
	timeout := e.Timeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 5 * time.Second
	}
	attempts := e.QueryAttempts
	if attempts <= 0 || attempts > 10 {
		attempts = 3
	}
	return timeout, attempts
}

func bounded(result CommandResult) bool {
	return len(result.Stdout) <= MaxCommandOutput && len(result.Stderr) <= MaxCommandOutput && len(result.Stdout)+len(result.Stderr) <= MaxCommandOutput
}

func classify(result CommandResult, runErr error, query bool) error {
	if ErrorCode(runErr) != "" {
		return runErr
	}
	text := strings.ToLower(string(result.Stderr))
	if runErr != nil {
		text += " " + strings.ToLower(runErr.Error())
	}
	switch {
	case strings.Contains(text, "not found") || strings.Contains(text, "no such file"):
		return fail("tool-missing")
	case strings.Contains(text, "permission denied") || strings.Contains(text, "operation not permitted"):
		return fail("permission-denied")
	case strings.Contains(text, "unrecognized option") || strings.Contains(text, "unknown option") || strings.Contains(text, "invalid option"):
		return fail("tool-version")
	case query && result.ExitCode != 0:
		return fail("query-nonzero")
	default:
		return fail("io-failed")
	}
}

func (e Executor) invoke(ctx context.Context, timeout time.Duration, args ...string) (CommandResult, error) {
	if e.Runner == nil {
		return CommandResult{}, fail("internal-error")
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := e.Runner.Run(callCtx, Invocation{Path: ConntrackPath, Args: append([]string(nil), args...), Env: append([]string(nil), fixedEnvironment...)})
	if !bounded(result) {
		return CommandResult{}, fail("output-invalid")
	}
	return result, err
}

// DirectRunner executes only the already validated absolute invocation. The
// Executor remains responsible for constructing it; no shell or PATH lookup is
// involved. Output is capped while it is being collected.
type DirectRunner struct{}

type limitedBuffer struct {
	data []byte
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	remaining := MaxCommandOutput + 1 - len(b.data)
	if remaining > 0 {
		if len(value) < remaining {
			remaining = len(value)
		}
		b.data = append(b.data, value[:remaining]...)
	}
	return len(value), nil
}

func (DirectRunner) Run(ctx context.Context, invocation Invocation) (CommandResult, error) {
	if !validFixedInvocation(invocation) {
		return CommandResult{}, errors.New("invalid fixed conntrack invocation")
	}
	command := exec.CommandContext(ctx, invocation.Path, invocation.Args...)
	command.Env = append([]string(nil), invocation.Env...)
	var stdout, stderr limitedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.data, Stderr: stderr.data}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func validFixedInvocation(invocation Invocation) bool {
	if invocation.Path != ConntrackPath || !reflect.DeepEqual(invocation.Env, fixedEnvironment) {
		return false
	}
	if reflect.DeepEqual(invocation.Args, []string{"--version"}) {
		return true
	}
	if len(invocation.Args) != 11 && len(invocation.Args) != 13 {
		return false
	}
	operation := invocation.Args[0]
	if operation != "-D" && operation != "-L" {
		return false
	}
	if operation == "-D" && len(invocation.Args) != 11 || operation == "-L" && (len(invocation.Args) != 13 || invocation.Args[11] != "-o" || invocation.Args[12] != "xml") {
		return false
	}
	port, err := strconv.Atoi(invocation.Args[8])
	if err != nil {
		return false
	}
	allocation, err := normalizeAllocation(Allocation{Address: invocation.Args[6], Protocol: invocation.Args[4], Port: port})
	return err == nil && reflect.DeepEqual(invocation.Args[1:11], tupleArgs(allocation))
}

func parseVersion(result CommandResult) (string, error) {
	if result.ExitCode != 0 {
		return "", classify(result, nil, false)
	}
	if len(result.Stderr) != 0 {
		return "", fail("output-invalid")
	}
	match := versionPattern.FindSubmatch(result.Stdout)
	if match == nil {
		return "", fail("tool-version")
	}
	patch, err := strconv.Atoi(string(match[1]))
	if err != nil || patch < 5 {
		return "", fail("tool-version")
	}
	return strings.TrimSuffix(string(result.Stdout), "\n"), nil
}

func parseDeleted(result CommandResult) (int, bool) {
	// conntrack-tools writes each deleted flow to stdout and its authoritative
	// deletion summary to stderr. Some builds emit only the summary on either
	// stream, so prefer stderr when present and otherwise accept stdout.
	body := result.Stderr
	if len(body) == 0 {
		body = result.Stdout
	}
	match := deletePattern.FindSubmatch(body)
	if match == nil {
		return 0, false
	}
	count, err := strconv.Atoi(string(match[1]))
	return count, err == nil && count >= 0
}

type conntrackDocument struct {
	XMLName xml.Name        `xml:"conntrack"`
	Flows   []conntrackFlow `xml:"flow"`
}

type conntrackFlow struct {
	Meta []conntrackMeta `xml:"meta"`
}

type conntrackMeta struct {
	Direction string          `xml:"direction,attr"`
	Layer3    conntrackLayer3 `xml:"layer3"`
	Layer4    conntrackLayer4 `xml:"layer4"`
}

type conntrackLayer3 struct {
	Protocol string `xml:"protoname,attr"`
	Dst      string `xml:"dst"`
}

type conntrackLayer4 struct {
	Protocol string `xml:"protoname,attr"`
	DstPort  int    `xml:"dport"`
}

func strictFlowCount(body []byte, allocation Allocation) (int, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	depth, flows := 0, 0
	closed := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !closed || depth != 0 {
				return 0, fail("output-invalid")
			}
			var document conntrackDocument
			if xml.Unmarshal(body, &document) != nil || len(document.Flows) != flows {
				return 0, fail("output-invalid")
			}
			for _, flow := range document.Flows {
				originals := 0
				for _, meta := range flow.Meta {
					if meta.Direction == "original" {
						originals++
						if meta.Layer3.Protocol != "ipv4" || meta.Layer3.Dst != allocation.Address || meta.Layer4.Protocol != allocation.Protocol || meta.Layer4.DstPort != allocation.Port {
							return 0, fail("output-invalid")
						}
					}
				}
				if originals != 1 {
					return 0, fail("output-invalid")
				}
			}
			return flows, nil
		}
		if err != nil {
			return 0, fail("output-invalid")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if closed || depth == 0 && value.Name.Local != "conntrack" || depth == 1 && value.Name.Local != "flow" {
				return 0, fail("output-invalid")
			}
			depth++
			if depth == 2 {
				flows++
			}
		case xml.EndElement:
			depth--
			if depth < 0 {
				return 0, fail("output-invalid")
			}
			if depth == 0 {
				closed = true
			}
		case xml.CharData:
			if depth < 2 && len(bytes.TrimSpace(value)) != 0 {
				return 0, fail("output-invalid")
			}
		case xml.Directive:
			return 0, fail("output-invalid")
		}
	}
}

func validateShown(stderr []byte, flows int) bool {
	if len(stderr) == 0 {
		return true
	}
	match := shownPattern.FindSubmatch(stderr)
	if match == nil {
		return false
	}
	count, err := strconv.Atoi(string(match[1]))
	return err == nil && count == flows
}

func validateExactShown(stderr []byte, version string, flows int) bool {
	want := version + ": " + strconv.Itoa(flows) + " flow entries have been shown."
	return string(stderr) == want || string(stderr) == want+"\n"
}

func tupleArgs(allocation Allocation) []string {
	return []string{"-f", "ipv4", "-p", allocation.Protocol, "--orig-dst", allocation.Address, "--dport", strconv.Itoa(allocation.Port), "--zone", "0"}
}

func (e Executor) Purge(ctx context.Context, input Allocation) (PurgeResult, error) {
	allocation, err := normalizeAllocation(input)
	if err != nil {
		return PurgeResult{}, err
	}
	timeout, attempts := e.settings()
	versionOutput, runErr := e.invoke(ctx, timeout, "--version")
	if runErr != nil {
		return PurgeResult{}, classify(versionOutput, runErr, false)
	}
	version, err := parseVersion(versionOutput)
	if err != nil {
		return PurgeResult{}, err
	}
	deleteArgs := append([]string{"-D"}, tupleArgs(allocation)...)
	deletedOutput, runErr := e.invoke(ctx, timeout, deleteArgs...)
	if runErr != nil {
		return PurgeResult{}, classify(deletedOutput, runErr, false)
	}
	deleted, validDelete := parseDeleted(deletedOutput)
	if !validDelete || deletedOutput.ExitCode != 0 && !(deletedOutput.ExitCode == 1 && deleted == 0) {
		if deletedOutput.ExitCode != 0 {
			return PurgeResult{}, classify(deletedOutput, nil, false)
		}
		return PurgeResult{}, fail("output-invalid")
	}
	queryArgs := append([]string{"-L"}, tupleArgs(allocation)...)
	queryArgs = append(queryArgs, "-o", "xml")
	for range attempts {
		queryOutput, queryErr := e.invoke(ctx, timeout, queryArgs...)
		if queryErr != nil || queryOutput.ExitCode != 0 {
			return PurgeResult{}, classify(queryOutput, queryErr, true)
		}
		flows := 0
		if len(queryOutput.Stdout) == 0 {
			// conntrack-tools 1.4.6 emits no XML document for an empty
			// filtered query. Accept that representation only when its exact,
			// authoritative summary independently says that zero entries were
			// shown; an absent, malformed, or non-zero summary remains fatal.
			if version != "conntrack v1.4.6 (conntrack-tools)" || !validateExactShown(queryOutput.Stderr, version, 0) {
				return PurgeResult{}, fail("output-invalid")
			}
		} else {
			var parseErr error
			flows, parseErr = strictFlowCount(queryOutput.Stdout, allocation)
			if parseErr != nil || !validateShown(queryOutput.Stderr, flows) {
				return PurgeResult{}, fail("output-invalid")
			}
		}
		if flows == 0 {
			return PurgeResult{ConntrackVersion: version, DeletedEntries: deleted}, nil
		}
	}
	return PurgeResult{}, fail("query-nonzero")
}
