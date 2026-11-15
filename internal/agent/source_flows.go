package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"

	"reladock/internal/sourceflow"
)

func (c *Client) FlushSourceFlows(ctx context.Context, wal *sourceflow.WAL) error {
	if wal == nil || c.HTTP == nil {
		return errors.New("source-flow WAL and mTLS client required")
	}
	if err := ValidateEndpoint(c.BaseURL, c.NodeID); err != nil {
		return err
	}
	batch, ok, err := wal.NextBatch()
	if err != nil || !ok {
		return err
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	if len(payload) > 512*1024 {
		return errors.New("source-flow batch exceeds request limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent/source-flows", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.do(req)
	if err != nil {
		return errors.New("source-flow upload connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("source-flow upload rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return errors.New("invalid source-flow receipt")
	}
	var receipt sourceflow.Receipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&receipt); err != nil || decoder.Decode(new(any)) != io.EOF || !reflect.DeepEqual(receipt, batch.Receipt()) {
		return errors.New("invalid source-flow receipt")
	}
	return wal.Ack(ctx, receipt)
}

func (e *NodeEngine) FlushSourceFlows(ctx context.Context, client *Client) error {
	if e == nil || !e.SourceFlowAvailable() {
		return errors.New("source-flow collection is not configured")
	}
	return client.FlushSourceFlows(ctx, e.SourceFlowWAL)
}
