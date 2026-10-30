package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"reladock/internal/platform"
)

type Applier interface {
	Apply(context.Context, platform.NodeConfig) error
}

// SafeApplyFailure is an explicit proof supplied by an Applier that every
// component has completed rollback and verified a stable durable/kernel state.
// Only this error permits a terminal failed report to release the applying
// fence. Unknown errors, cancellation, and uncertain recovery retain the fence.
// Composition must cover nft, tc and the collector/WAL before issuing this proof.
type SafeApplyFailure struct{ Err error }

func (e *SafeApplyFailure) Error() string { return "application failed; stable rollback verified" }
func (e *SafeApplyFailure) Unwrap() error { return e.Err }

const DefaultApplyTimeout = 25 * time.Second
const reportReserve = 5 * time.Second

var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// A rejection records an actual current HTTP response, not transport errors or
// response text. Only an unconfirmed begin's explicit 409 proves that its exact
// job/token is not applying: the backend returns 200 for an applying exact token.
type controlRejection struct {
	operation string
	status    int
}

func (e *controlRejection) Error() string {
	return fmt.Sprintf("%s rejected: HTTP %d", e.operation, e.status)
}

type Client struct {
	HTTP    *http.Client
	BaseURL string
	NodeID  string
	// Nil Applier preserves validation-only operation, without begin or state I/O.
	Applier      Applier
	State        *ApplyState
	ApplyTimeout time.Duration
	Now          func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func (c *Client) applyTimeout() time.Duration {
	if c.ApplyTimeout == 0 {
		return DefaultApplyTimeout
	}
	return c.ApplyTimeout
}

func (c *Client) Once(ctx context.Context) (bool, error) {
	if err := ValidateEndpoint(c.BaseURL, c.NodeID); err != nil {
		return false, err
	}
	if c.HTTP == nil {
		return false, errors.New("mTLS HTTP client is required")
	}
	if c.Applier != nil {
		if c.State == nil {
			return false, errors.New("apply mode requires durable state and process lock")
		}
		if c.applyTimeout() <= 0 || c.applyTimeout() > 45*time.Second {
			return false, errors.New("apply timeout must be positive and at most 45 seconds")
		}
		unlock, err := c.State.Lock(ctx)
		if err != nil {
			return false, err
		}
		defer unlock()
		record, err := c.State.load(ctx)
		if err != nil {
			return false, err
		}
		if record != nil {
			if record.Origin != c.BaseURL || record.NodeID != c.NodeID || !validEnvelope(record.Job) {
				return false, errors.New("pending delivery identity mismatch; explicit reconciliation required")
			}
			if record.Report != nil {
				r := record.Report
				if r.LeaseToken != record.Job.LeaseToken || r.Checksum != record.Job.Checksum || (r.Outcome != "applied" && r.Outcome != "failed") {
					return false, errors.New("invalid pending report")
				}
				if r.Outcome == "applied" && (r.ErrorCode != "" || ValidateConfig(c.NodeID, record.Job) != nil) {
					return false, errors.New("persisted applied receipt failed integrity validation")
				}
				if r.Outcome == "failed" && record.BeginAttempted && !record.FailureStable {
					return false, errors.New("pending failure lacks stable rollback proof; journal and application fence retained")
				}
				return c.reportAndClear(ctx, record, nil)
			}
			return c.apply(ctx, record)
		}
		return c.claim(ctx)
	}
	return c.claim(ctx)
}

func validEnvelope(j platform.ConfigJob) bool {
	return jobIDPattern.MatchString(j.ID) && j.LeaseToken != "" && len(j.LeaseToken) <= 256
}
func (c *Client) claim(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/agent/config/claim", nil)
	if err != nil {
		return false, err
	}
	response, err := c.do(req)
	if err != nil {
		return false, errors.New("configuration claim unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 204 {
		return false, nil
	}
	if response.StatusCode != 200 {
		return false, fmt.Errorf("claim rejected: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (3<<20)+1))
	if err != nil {
		return false, err
	}
	if len(body) > 3<<20 {
		return false, errors.New("oversized config response")
	}
	var job platform.ConfigJob
	if err = json.Unmarshal(body, &job); err != nil {
		return false, errors.New("invalid config response")
	}
	if !validEnvelope(job) {
		return false, errors.New("invalid configuration delivery envelope")
	}
	validationErr := ValidateConfig(c.NodeID, job)
	report := platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "validated"}
	if validationErr != nil {
		report.Outcome = "failed"
		report.ErrorCode = "invalid-config"
	}
	if c.Applier == nil {
		if err = c.post(ctx, job, "report", report); err != nil {
			return false, err
		}
		return true, validationErr
	}
	record := &applyRecord{Origin: c.BaseURL, NodeID: c.NodeID, Job: job}
	if validationErr != nil {
		record.Report = &report
	}
	if err = c.State.save(ctx, record); err != nil {
		return false, err
	}
	if validationErr != nil {
		return c.reportAndClear(ctx, record, validationErr)
	}
	return c.apply(ctx, record)
}
func (c *Client) apply(ctx context.Context, record *applyRecord) (bool, error) {
	job := record.Job
	if err := ValidateConfig(c.NodeID, job); err != nil {
		return false, errors.New("persisted configuration failed validation")
	}
	// The backend's applying fence never expires. A persisted confirmed begin
	// can be resumed after expiry. For a lost begin response, wait for expiry:
	// then begin=200 proves it was already applying (an expired leased job
	// cannot newly begin). Initial applications always need the full budget.
	recoverFence := record.BeginConfirmed || record.BeginAttempted && !job.LeaseUntil.After(c.now())
	if !recoverFence && job.LeaseUntil.Sub(c.now()) < c.applyTimeout()+reportReserve {
		cause := errors.New("insufficient lease remaining for safe application; no adapter call")
		// Claim was journaled, but without even an attempted begin neither the
		// server fence nor adapter execution could have started. Drop this local
		// receipt so a later Once can claim again after the server lease expires.
		// Attempted/confirmed begins must remain until an explicit safe result.
		if !record.BeginAttempted && !record.BeginConfirmed {
			if err := c.State.clear(); err != nil {
				return false, errors.Join(cause, err)
			}
		}
		return false, cause
	}
	if !record.BeginAttempted {
		record.BeginAttempted = true
		if err := c.State.save(ctx, record); err != nil {
			return false, err
		}
	}
	beginBudget := 20 * time.Second
	if !recoverFence {
		beginBudget = job.LeaseUntil.Sub(c.now()) - c.applyTimeout() - reportReserve
	}
	beginCtx, cancel := context.WithTimeout(ctx, beginBudget)
	err := c.post(beginCtx, job, "begin", platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applying"})
	cancel()
	if err != nil {
		var rejected *controlRejection
		if !record.BeginConfirmed && errors.As(err, &rejected) && rejected.operation == "begin" && rejected.status == http.StatusConflict {
			// A prior lost response alone is uncertain. This current exact-token
			// 409 resolves it as not applying, so no adapter/report has run. 401,
			// network failures, 5xx and every confirmed begin retain the journal.
			if clearErr := c.State.clear(); clearErr != nil {
				return false, errors.Join(err, clearErr)
			}
		}
		return false, err
	}
	record.BeginConfirmed = true
	if err = c.State.save(ctx, record); err != nil {
		return false, err
	}
	if !recoverFence && job.LeaseUntil.Sub(c.now()) < c.applyTimeout()+reportReserve {
		return false, errors.New("lease consumed by begin; refusing application")
	}
	var cfg platform.NodeConfig
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		return false, err
	}
	applyCtx, cancel := context.WithTimeout(ctx, c.applyTimeout())
	applyErr := c.Applier.Apply(applyCtx, cfg)
	cancel()
	if applyErr != nil {
		var safe *SafeApplyFailure
		if !errors.As(applyErr, &safe) {
			// The original begin and job are already durable. Do not manufacture
			// a terminal result or clear the journal: Recover/reconciliation and
			// an exact verified retry must resolve any uncertain kernel work.
			return false, fmt.Errorf("application fence retained pending recovery: %w", applyErr)
		}
		record.FailureStable = true
	}
	report := platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applied"}
	if applyErr != nil {
		report.Outcome = "failed"
		report.ErrorCode = "internal-error"
	}
	record.Report = &report
	// Persist the exact result before sending it; ACK loss cannot rerun application.
	// If this save fails, the durable job requires ApplyVerified's exact recheck.
	if err = c.State.save(context.WithoutCancel(ctx), record); err != nil {
		return false, err
	}
	if applyErr != nil {
		applyErr = errors.New("local configuration application failed")
	}
	return c.reportAndClear(ctx, record, applyErr)
}
func (c *Client) reportAndClear(ctx context.Context, record *applyRecord, cause error) (bool, error) {
	if err := c.post(ctx, record.Job, "report", *record.Report); err != nil {
		return false, err
	}
	if err := c.State.clear(); err != nil {
		return false, err
	}
	return true, cause
}
func (c *Client) do(req *http.Request) (*http.Response, error) {
	client := *c.HTTP
	if client.Timeout <= 0 || client.Timeout > 20*time.Second {
		client.Timeout = 20 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	version := "validation-only/0.7.7"
	if c.Applier != nil {
		version = "apply-opt-in/0.7.7"
	}
	req.Header.Set("X-RelaDock-Agent-Version", version)
	return client.Do(req)
}
func (c *Client) post(ctx context.Context, job platform.ConfigJob, suffix string, report platform.ConfigReport) error {
	payload, _ := json.Marshal(report)
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/agent/config/"+job.ID+"/"+suffix, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := c.do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
			response.Body.Close()
			if response.StatusCode == 200 {
				return nil
			}
			if response.StatusCode < 500 {
				return &controlRejection{operation: suffix, status: response.StatusCode}
			}
		}
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%s acknowledgement unavailable; exact retry retained", suffix)
}

func ValidateEndpoint(endpoint, node string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return errors.New("agent endpoint must be an HTTPS origin")
	}
	if !nodeIDPattern.MatchString(node) {
		return errors.New("invalid node identity")
	}
	return nil
}
