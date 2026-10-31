package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"reladock/internal/buildinfo"
	"reladock/internal/platform"
)

func UpdaterStable(statePath string) bool {
	_, err := os.Lstat(statePath)
	return errors.Is(err, os.ErrNotExist)
}

func (e *NodeEngine) BuildDrainAttestation(ctx context.Context, state *RetirementState, fingerprint, bootID string, startedAt time.Time, updaterStable bool) (platform.DrainAttestation, error) {
	if e == nil || !nodeIDPattern.MatchString(e.NodeID) || e.Adapter == nil || e.WAL == nil || state == nil || !nodeauthFingerprint(fingerprint) || !jobIDPattern.MatchString(bootID) || startedAt.IsZero() || !updaterStable {
		return platform.DrainAttestation{}, errors.New("drain evidence unavailable")
	}
	if journal, err := state.Load(ctx); err != nil || journal != nil {
		return platform.DrainAttestation{}, errors.New("retirement journal not empty")
	}
	if _, pending, err := e.WAL.Next(); err != nil || pending {
		return platform.DrainAttestation{}, errors.New("usage WAL not drained")
	}
	evidence, err := e.Adapter.DrainEvidence(ctx)
	if err != nil {
		return platform.DrainAttestation{}, err
	}
	sequence, err := state.NextSequence(ctx, time.Now().UTC().UnixNano())
	if err != nil {
		return platform.DrainAttestation{}, err
	}
	return platform.DrainAttestation{SchemaVersion: 1, NodeID: e.NodeID, Generation: evidence.Generation, CertificateFingerprint: fingerprint, KernelBootID: bootID, AgentStartedAt: startedAt.UTC(), AgentVersion: buildinfo.Current(), JournalSequence: sequence, GoodDigest: evidence.GoodDigest, QuarantineCount: evidence.QuarantineCount, QuarantineDigest: evidence.QuarantineDigest, NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now().UTC()}, nil
}

var drainFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

func nodeauthFingerprint(value string) bool { return drainFingerprint.MatchString(value) }

func (c *Client) DrainRequested(ctx context.Context) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent/drain-status", nil)
	if err != nil {
		return false, err
	}
	response, err := c.do(request)
	if err != nil {
		return false, errors.New("drain status unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
		return false, &controlRejection{operation: "drain-status", status: response.StatusCode}
	}
	var value struct {
		Draining bool `json:"draining"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1024))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&value); err != nil {
		return false, errors.New("invalid drain status")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return false, errors.New("invalid drain status")
	}
	return value.Draining, nil
}

func (c *Client) ReportDrainAttestation(ctx context.Context, value platform.DrainAttestation) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 8*1024 {
		return errors.New("invalid drain attestation")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent/drain-attestation", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		return errors.New("drain attestation unavailable")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	if response.StatusCode != http.StatusOK {
		return &controlRejection{operation: "drain-attestation", status: response.StatusCode}
	}
	return nil
}
