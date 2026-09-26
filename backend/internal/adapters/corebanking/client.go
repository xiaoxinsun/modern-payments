// Package corebanking calls the Core Banking port over authenticated HTTP. It
// performs no retries: callers decide whether an operation is replay-safe.
package corebanking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gtb.local/backend/internal/payment/application"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
)

// Client needs an HTTP client whose transport attaches a service token with
// the core-banking-service audience and internal_service role.
type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return nil, errors.New("Core Banking URL must be an absolute HTTP(S) URL without credentials")
	}
	if httpClient == nil {
		return nil, errors.New("Core Banking HTTP client is required")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}, nil
}

type accountResponse struct {
	AccountID  value.ID       `json:"accountId"`
	CustomerID *value.ID      `json:"customerId"`
	Kind       string         `json:"accountKind"`
	Currency   value.Currency `json:"currency"`
	Status     string         `json:"status"`
}

// Account implements application.AccountDirectory. Any response other than a
// decoded 200 or a 404 means ownership could not be established.
func (client *Client) Account(ctx context.Context, accountID value.ID) (application.Account, error) {
	if err := accountID.Validate(); err != nil {
		return application.Account{}, application.ErrAccountNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/integrations/v1/core-banking/accounts/"+string(accountID), nil)
	if err != nil {
		return application.Account{}, err
	}
	if correlationID := platformruntime.CorrelationID(ctx); correlationID != "" {
		request.Header.Set(platformruntime.CorrelationHeader, string(correlationID))
	}
	response, err := client.http.Do(request)
	if err != nil {
		return application.Account{}, fmt.Errorf("%w: Core Banking account lookup failed", application.ErrDependencyUnavailable)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return application.Account{}, application.ErrAccountNotFound
	default:
		return application.Account{}, fmt.Errorf("%w: Core Banking returned HTTP %d", application.ErrDependencyUnavailable, response.StatusCode)
	}
	var account accountResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&account); err != nil || account.AccountID != accountID {
		return application.Account{}, fmt.Errorf("%w: invalid Core Banking account response", application.ErrDependencyUnavailable)
	}
	return application.Account{
		AccountID: account.AccountID, CustomerID: account.CustomerID, Kind: account.Kind,
		Currency: account.Currency, Status: account.Status,
	}, nil
}
