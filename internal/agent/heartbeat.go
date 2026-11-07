package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"reladock/internal/platform"
)

func (c *Client) ReportHeartbeat(ctx context.Context, heartbeat platform.AgentHeartbeat) error {
	if err := ValidateEndpoint(c.BaseURL, c.NodeID); err != nil {
		return err
	}
	if c.HTTP == nil {
		return errors.New("mTLS client required")
	}
	payload, err := json.Marshal(heartbeat)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent/heartbeat", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if heartbeat.CertificateRotationSupported {
		req.Header.Set("X-RelaDock-Certificate-Rotation", "1")
	}
	response, err := c.do(req)
	if err != nil {
		return errors.New("heartbeat connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("heartbeat rejected")
	}
	var reply struct {
		OK                            bool   `json:"ok"`
		CertificateRenewalFingerprint string `json:"certificateRenewalFingerprint"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(raw) > 8192 {
		return errors.New("invalid heartbeat response")
	}
	if err = json.Unmarshal(raw, &reply); err != nil || !reply.OK {
		return errors.New("invalid heartbeat response")
	}
	if reply.CertificateRenewalFingerprint != "" && !renewalFingerprintPattern.MatchString(reply.CertificateRenewalFingerprint) {
		return errors.New("invalid certificate renewal request")
	}
	if c.Credentials != nil {
		return c.Credentials.RequestRotation(reply.CertificateRenewalFingerprint)
	}
	return nil
}

// ReportRuntimeHeartbeat attaches one bounded local observation immediately
// before transport. The observation is operational evidence, not path health.
func (c *Client) ReportRuntimeHeartbeat(ctx context.Context, heartbeat platform.AgentHeartbeat, collector *ResourceCollector, inspector DataplaneInspector) error {
	if collector == nil {
		return errors.New("resource collector required")
	}
	heartbeat.Resources = collector.Collect(ctx, inspector)
	return c.ReportHeartbeat(ctx, heartbeat)
}
