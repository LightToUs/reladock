package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
	retirementexec "reladock/internal/retirement"
)

type RetirementWorker struct {
	State   *RetirementState
	Adapter RetirementAdapter
	BootID  string
	Now     func() time.Time
}

type RetirementAdapter interface {
	Recover(context.Context) error
	PurgeAllocation(context.Context, platform.RetirementClaim) (dataplane.PurgeOutcome, error)
	ConfirmRetirementACK(context.Context, string, string) error
}

func ReadKernelBootID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if !jobIDPattern.MatchString(value) {
		return "", errors.New("invalid kernel boot identity")
	}
	return value, nil
}

func (w *RetirementWorker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func validRetirementClaim(claim platform.RetirementClaim, nodeID string) bool {
	address, err := netip.ParseAddr(claim.Address)
	return err == nil && address.Is4() && address.IsGlobalUnicast() && !address.IsLoopback() && address.String() == claim.Address && jobIDPattern.MatchString(claim.ID) && jobIDPattern.MatchString(claim.AllocationID) && configIdentifier.MatchString(claim.RuleID) && claim.NodeID == nodeID && nodeIDPattern.MatchString(claim.NodeID) && len(claim.LeaseToken) == 64 && !claim.LeaseUntil.IsZero() && claim.QuarantineGeneration >= 1 && (claim.Protocol == "tcp" || claim.Protocol == "udp") && claim.Port >= 20000 && claim.Port <= 60000
}

// Once is called while Client holds the shared external control lock. It
// resolves an existing journal before claiming new work and before config apply.
func (w *RetirementWorker) Once(ctx context.Context, client *Client) (bool, error) {
	if w == nil || w.State == nil || w.Adapter == nil || !jobIDPattern.MatchString(w.BootID) {
		return false, errors.New("retirement worker is not safely configured")
	}
	record, err := w.State.Load(ctx)
	if err != nil {
		return false, err
	}
	if record == nil {
		claim, claimErr := claimRetirement(ctx, client)
		if claimErr != nil || claim == nil {
			return false, claimErr
		}
		if !validRetirementClaim(*claim, client.NodeID) || !claim.LeaseUntil.After(w.now()) {
			return false, errors.New("invalid retirement claim")
		}
		sequence, sequenceErr := w.State.NextSequence(ctx, w.now().UnixNano())
		if sequenceErr != nil {
			return false, sequenceErr
		}
		value := retirementRecord{Origin: client.BaseURL, NodeID: client.NodeID, Claim: *claim, Sequence: sequence}
		if err = w.State.Save(ctx, value); err != nil {
			return false, err
		}
		record = &value
	}
	if record.Origin != client.BaseURL || record.NodeID != client.NodeID || !validRetirementClaim(record.Claim, client.NodeID) || record.Sequence < 1 {
		return false, errors.New("retirement journal identity mismatch; explicit reconciliation required")
	}
	if record.Proof == nil {
		if !record.BeginAttempted {
			record.BeginAttempted = true
			if err = w.State.Save(ctx, *record); err != nil {
				return false, err
			}
		}
		if !record.BeginConfirmed {
			if err = postRetirement(ctx, client, "begin", map[string]string{"taskId": record.Claim.ID, "leaseToken": record.Claim.LeaseToken}); err != nil {
				var rejected *controlRejection
				if errors.As(err, &rejected) && rejected.operation == "retirement-begin" && rejected.status == http.StatusConflict {
					if clearErr := w.State.Clear(); clearErr != nil {
						return false, errors.Join(err, clearErr)
					}
				}
				return false, err
			}
			record.BeginConfirmed = true
			if err = w.State.Save(ctx, *record); err != nil {
				return false, err
			}
		}
		if err = w.Adapter.Recover(ctx); err != nil {
			return false, w.reportFailure(ctx, client, record, err)
		}
		outcome, purgeErr := w.Adapter.PurgeAllocation(ctx, record.Claim)
		if purgeErr != nil {
			return false, w.reportFailure(ctx, client, record, purgeErr)
		}
		proof, _, _, proofErr := retirementexec.BuildProof(retirementexec.ProofInput{
			TaskID: record.Claim.ID, AllocationID: record.Claim.AllocationID, NodeID: record.Claim.NodeID, Address: record.Claim.Address,
			Protocol: record.Claim.Protocol, Port: record.Claim.Port, QuarantineGeneration: record.Claim.QuarantineGeneration,
			GuardBeforeDigest: outcome.GuardBeforeDigest, GuardAfterDigest: outcome.GuardAfterDigest, ConntrackVersion: outcome.ConntrackVersion,
			DeletedEntries: outcome.DeletedEntries, KernelBootID: w.BootID, JournalSequence: record.Sequence, GoodDigest: outcome.GoodDigest, ObservedAt: w.now(),
		})
		if proofErr != nil {
			return false, w.reportFailure(ctx, client, record, proofErr)
		}
		record.Proof = &proof
		if err = w.State.Save(context.WithoutCancel(ctx), *record); err != nil {
			return false, err
		}
	}
	report := platform.RetirementReport{LeaseToken: record.Claim.LeaseToken, Proof: *record.Proof}
	if err = postRetirement(ctx, client, "report", map[string]any{"taskId": record.Claim.ID, "report": report}); err != nil {
		return false, err
	}
	if err = w.Adapter.ConfirmRetirementACK(context.WithoutCancel(ctx), record.Claim.ID, record.Claim.AllocationID); err != nil {
		return false, err
	}
	if err = w.State.Clear(); err != nil {
		return false, err
	}
	return true, nil
}

func (w *RetirementWorker) reportFailure(ctx context.Context, client *Client, record *retirementRecord, cause error) error {
	code := dataplane.RetirementErrorCode(cause)
	if code == "" {
		code = "internal-error"
	}
	reportErr := postRetirement(ctx, client, "report", map[string]string{"taskId": record.Claim.ID, "leaseToken": record.Claim.LeaseToken, "errorCode": code})
	if reportErr != nil {
		return errors.Join(errors.New("retirement failed; journal and quarantine retained"), reportErr)
	}
	return errors.New("retirement failed; platform notified and journal retained")
}

func claimRetirement(ctx context.Context, client *Client) (*platform.RetirementClaim, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.BaseURL+"/agent/retirements/claim", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.do(req)
	if err != nil {
		return nil, errors.New("retirement claim unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("retirement claim rejected: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return nil, errors.New("invalid retirement claim response")
	}
	var claim platform.RetirementClaim
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&claim); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid retirement claim response")
	}
	return &claim, nil
}

func postRetirement(ctx context.Context, client *Client, operation string, value any) error {
	raw, _ := json.Marshal(value)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.BaseURL+"/agent/retirements/"+operation, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.do(request)
	if err != nil {
		return fmt.Errorf("retirement %s unavailable; exact retry retained", operation)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	if response.StatusCode != http.StatusOK {
		return &controlRejection{operation: "retirement-" + operation, status: response.StatusCode}
	}
	return nil
}
