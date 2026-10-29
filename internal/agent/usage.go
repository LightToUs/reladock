package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reladock/internal/metering"
	"reladock/internal/platform"
)

func (c *Client) FlushUsage(ctx context.Context, w *metering.WAL) error {
	if e := ValidateEndpoint(c.BaseURL, c.NodeID); e != nil {
		return e
	}
	if c.HTTP == nil || w == nil {
		return errors.New("mTLS client and WAL required")
	}
	return w.Flush(ctx, func(ctx context.Context, payload []byte) (metering.Receipt, error) {
		req, e := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/agent/usage", bytes.NewReader(payload))
		if e != nil {
			return metering.Receipt{}, e
		}
		req.Header.Set("Content-Type", "application/json")
		resp, e := c.do(req)
		if e != nil {
			return metering.Receipt{}, errors.New("usage acknowledgement unavailable")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return metering.Receipt{}, fmt.Errorf("usage report rejected: HTTP %d", resp.StatusCode)
		}
		body, e := io.ReadAll(io.LimitReader(resp.Body, 8193))
		if e != nil || len(body) > 8192 {
			return metering.Receipt{}, errors.New("invalid usage response")
		}
		var r platform.UsageReceipt
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		if e = d.Decode(&r); e != nil {
			return metering.Receipt{}, errors.New("invalid usage receipt")
		}
		if d.Decode(new(any)) != io.EOF {
			return metering.Receipt{}, errors.New("trailing usage receipt")
		}
		return metering.Receipt{RuleID: r.RuleID, SubscriptionID: r.SubscriptionID, Epoch: r.Epoch, Sequence: r.Sequence}, nil
	})
}
