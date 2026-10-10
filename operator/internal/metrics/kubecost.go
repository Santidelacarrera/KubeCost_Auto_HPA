package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// SpendSource returns the month-to-date cost of a namespace in USD.
type SpendSource interface {
	MonthlySpend(ctx context.Context, namespace string) (float64, error)
}

// KubeCost reads the KubeCost allocation API.
type KubeCost struct {
	BaseURL string
	HTTP    *http.Client
}

// NewKubeCost builds a client with a bounded timeout.
func NewKubeCost(baseURL string) *KubeCost {
	return &KubeCost{BaseURL: baseURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

type allocationResponse struct {
	Code int `json:"code"`
	Data []map[string]struct {
		TotalCost float64 `json:"totalCost"`
	} `json:"data"`
}

// MonthlySpend returns totalCost for the namespace over window=month.
func (k *KubeCost) MonthlySpend(ctx context.Context, namespace string) (float64, error) {
	q := url.Values{"window": {"month"}, "aggregate": {"namespace"}, "accumulate": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.BaseURL+"/model/allocation?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("kubecost returned %d", resp.StatusCode)
	}
	var ar allocationResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return 0, err
	}
	for _, set := range ar.Data {
		if a, ok := set[namespace]; ok {
			return a.TotalCost, nil
		}
	}
	return 0, ErrNoData
}
