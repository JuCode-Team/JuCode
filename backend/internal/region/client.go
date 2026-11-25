package region

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Client is the HTTP implementation of the control-plane API, used by edges.
// A non-2xx answer comes back as *Error, which matches the sentinel of its
// code under errors.Is.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

func (c *Client) doJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+APIPrefix+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(respBody))}
		var body ErrorResponse
		if json.Unmarshal(respBody, &body) == nil && body.Error.Code != "" {
			apiErr.Code, apiErr.Message = body.Error.Code, body.Error.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode regional control response: %w", err)
	}
	return nil
}

func (c *Client) FetchAccount(ctx context.Context, apiKeyHash string) (*AccountSnapshot, error) {
	var out AccountSnapshotResponse
	err := c.doJSON(ctx, PathAccountSnapshot, AccountSnapshotRequest{APIKeyHash: apiKeyHash}, &out)
	if err != nil {
		return nil, err
	}
	return &out.Account, nil
}

// FetchProviders pulls the upstream snapshot from the control plane. The method
// and the request key keep their historical names (the endpoint and its JSON are
// the frozen edge protocol); the ids passed, and the IDs in the returned
// snapshots, are upstream ACCOUNT ids.
func (c *Client) FetchProviders(ctx context.Context, accountIDs []string) (*ProviderSnapshotResponse, error) {
	var out ProviderSnapshotResponse
	err := c.doJSON(ctx, PathProviderSnapshot, ProviderSnapshotRequest{ProviderIDs: accountIDs}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GrantLease(ctx context.Context, req CreditLeaseRequest) (*CreditLease, error) {
	var out CreditLeaseResponse
	if err := c.doJSON(ctx, PathCreditLease, req, &out); err != nil {
		return nil, err
	}
	return &out.Lease, nil
}

func (c *Client) ReleaseLease(ctx context.Context, leaseID uuid.UUID) error {
	return c.doJSON(ctx, PathCreditLeaseRelease, CreditLeaseReleaseRequest{LeaseID: leaseID}, nil)
}

func (c *Client) PushUsage(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	return c.doJSON(ctx, PathUsageEvents, UsageEventsRequest{Events: events}, nil)
}
