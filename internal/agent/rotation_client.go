package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"reladock/internal/platform"
)

func (c *Client) Renew(ctx context.Context, csr []byte) (platform.CertificateRotation, error) {
	result := platform.CertificateRotation{}
	if err := ValidateEndpoint(c.BaseURL, c.NodeID); err != nil {
		return result, err
	}
	if c.HTTP == nil {
		return result, errors.New("mTLS client required")
	}
	payload, _ := json.Marshal(map[string]string{"csr": string(csr)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent/certificate/renew", bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.do(req)
	if err != nil {
		return result, errors.New("certificate renewal connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, errors.New("certificate renewal rejected")
	}
	var body struct {
		Certificate string    `json:"certificate"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 65537))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil || body.Certificate == "" || body.ExpiresAt.IsZero() {
		return result, errors.New("invalid certificate renewal response")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return result, errors.New("invalid certificate renewal response")
	}
	return platform.CertificateRotation{Certificate: []byte(body.Certificate), ExpiresAt: body.ExpiresAt}, nil
}

func (c *Client) Activate(ctx context.Context, pending *tls.Certificate) error {
	if err := ValidateEndpoint(c.BaseURL, c.NodeID); err != nil {
		return err
	}
	if c.HTTP == nil || pending == nil || len(pending.Certificate) == 0 {
		return errors.New("certificate activation transport unavailable")
	}
	transport, ok := c.HTTP.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return errors.New("certificate activation transport unavailable")
	}
	temporary := transport.Clone()
	temporary.TLSClientConfig = transport.TLSClientConfig.Clone()
	pair := *pending
	temporary.TLSClientConfig.Certificates = nil
	temporary.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &pair, nil }
	defer temporary.CloseIdleConnections()
	client := &http.Client{Transport: temporary, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent/certificate/activate", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-RelaDock-Agent-Version", "rotation/0.8.0")
	response, err := client.Do(req)
	if err != nil {
		return errors.New("certificate activation connection failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	if response.StatusCode != http.StatusOK {
		return errors.New("certificate activation rejected")
	}
	return nil
}
