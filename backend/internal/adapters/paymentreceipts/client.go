// Package paymentreceipts reads the payment command store's receipt for a
// payment the query models have not projected yet. It forwards the caller's
// own bearer token, so the payment API applies its authorization, and it
// performs no retries.
package paymentreceipts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections/query"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return nil, errors.New("payment API URL must be an absolute HTTP(S) URL without credentials")
	}
	if httpClient == nil {
		return nil, errors.New("payment receipt HTTP client is required")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}, nil
}

type paymentRecord struct {
	PaymentID        value.ID `json:"paymentId"`
	CustomerID       value.ID `json:"customerId"`
	Version          string   `json:"version"`
	LifecycleState   string   `json:"lifecycleState"`
	ExecutionOutcome string   `json:"executionOutcome"`
	Instruction      struct {
		Product string      `json:"product"`
		Amount  value.Money `json:"amount"`
	} `json:"instruction"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// Receipt implements query.Receipts.
func (client *Client) Receipt(ctx context.Context, authorization string, paymentID value.ID) (query.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/api/v1/payments/"+string(paymentID)+"/receipt", nil)
	if err != nil {
		return query.Receipt{}, err
	}
	request.Header.Set("Authorization", authorization)
	if correlationID := platformruntime.CorrelationID(ctx); correlationID != "" {
		request.Header.Set(platformruntime.CorrelationHeader, string(correlationID))
	}
	response, err := client.http.Do(request)
	if err != nil {
		return query.Receipt{}, fmt.Errorf("%w: receipt lookup failed", query.ErrDependencyUnavailable)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return query.Receipt{}, query.ErrNotFound
	case http.StatusForbidden:
		return query.Receipt{}, query.ErrForbidden
	default:
		return query.Receipt{}, fmt.Errorf("%w: payment API returned HTTP %d", query.ErrDependencyUnavailable, response.StatusCode)
	}
	var record paymentRecord
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&record); err != nil || record.PaymentID != paymentID {
		return query.Receipt{}, fmt.Errorf("%w: invalid receipt response", query.ErrDependencyUnavailable)
	}
	version, err := strconv.ParseInt(record.Version, 10, 64)
	if err != nil || version < 1 {
		return query.Receipt{}, fmt.Errorf("%w: invalid receipt version", query.ErrDependencyUnavailable)
	}
	return query.Receipt{
		PaymentID: record.PaymentID, CustomerID: record.CustomerID, Product: record.Instruction.Product,
		Amount: record.Instruction.Amount, LifecycleState: record.LifecycleState, ExecutionOutcome: record.ExecutionOutcome,
		Version: version, ReceivedAt: record.ReceivedAt.UTC(),
	}, nil
}
