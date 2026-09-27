// Package paymentjournal reads the payment event journal over authenticated
// HTTP for projection gap catch-up and rebuild. It performs no retries.
package paymentjournal

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

	"gtb.local/backend/internal/contracts"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	"gtb.local/backend/internal/platform/value"
	"gtb.local/backend/internal/projections"
)

var ErrUnavailable = errors.New("payment journal unavailable")

// Client needs an HTTP client whose transport attaches a service token with
// the payment-api audience and the internal_service role.
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
		return nil, errors.New("payment journal HTTP client is required")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}, nil
}

func (client *Client) Watermark(ctx context.Context) (projections.JournalWatermark, error) {
	var body struct {
		Position string `json:"position"`
		Events   string `json:"events"`
		Payments string `json:"payments"`
	}
	if err := client.get(ctx, "/integrations/v1/payment-events/watermark", &body); err != nil {
		return projections.JournalWatermark{}, err
	}
	var watermark projections.JournalWatermark
	for target, raw := range map[*int64]string{&watermark.Position: body.Position, &watermark.Events: body.Events, &watermark.Payments: body.Payments} {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return projections.JournalWatermark{}, fmt.Errorf("%w: invalid watermark response", ErrUnavailable)
		}
		*target = parsed
	}
	return watermark, nil
}

func (client *Client) Page(ctx context.Context, afterPosition, upToPosition int64, limit int) ([]projections.JournalEntry, error) {
	query := url.Values{
		"afterPosition": {strconv.FormatInt(afterPosition, 10)}, "upToPosition": {strconv.FormatInt(upToPosition, 10)},
		"limit": {strconv.Itoa(limit)},
	}
	return client.entries(ctx, "/integrations/v1/payment-events?"+query.Encode())
}

func (client *Client) PaymentEvents(ctx context.Context, paymentID value.ID, afterSequence int64, limit int) ([]projections.JournalEntry, error) {
	if err := paymentID.Validate(); err != nil {
		return nil, err
	}
	query := url.Values{"afterSequence": {strconv.FormatInt(afterSequence, 10)}, "limit": {strconv.Itoa(limit)}}
	return client.entries(ctx, "/integrations/v1/payments/"+string(paymentID)+"/events?"+query.Encode())
}

func (client *Client) entries(ctx context.Context, path string) ([]projections.JournalEntry, error) {
	var body struct {
		Entries []struct {
			Position string          `json:"position"`
			Event    contracts.Event `json:"event"`
		} `json:"entries"`
	}
	if err := client.get(ctx, path, &body); err != nil {
		return nil, err
	}
	entries := make([]projections.JournalEntry, len(body.Entries))
	for index, entry := range body.Entries {
		position, err := strconv.ParseInt(entry.Position, 10, 64)
		if err != nil || position < 1 {
			return nil, fmt.Errorf("%w: invalid journal position", ErrUnavailable)
		}
		entries[index] = projections.JournalEntry{Position: position, Event: entry.Event}
	}
	return entries, nil
}

func (client *Client) get(ctx context.Context, path string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+path, nil)
	if err != nil {
		return err
	}
	if correlationID := platformruntime.CorrelationID(ctx); correlationID != "" {
		request.Header.Set(platformruntime.CorrelationHeader, string(correlationID))
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: payment API returned HTTP %d", ErrUnavailable, response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target); err != nil {
		return fmt.Errorf("%w: undecodable journal response", ErrUnavailable)
	}
	return nil
}
