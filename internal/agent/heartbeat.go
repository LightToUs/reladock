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
	response, err := c.do(req)
	if err != nil {
		return errors.New("heartbeat connection failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	if response.StatusCode != http.StatusOK {
		return errors.New("heartbeat rejected")
	}
	return nil
}
