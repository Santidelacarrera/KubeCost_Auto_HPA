// Package engine is the client for the Python Unit Economics engine.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Budget carries month-to-date spend against the limit.
type Budget struct {
	MonthlyLimit float64 `json:"monthly_limit"`
	Spent        float64 `json:"spent"`
}

// Spot describes the workload's Spot eligibility inputs.
type Spot struct {
	FaultTolerant bool `json:"fault_tolerant"`
	Replicas      int  `json:"replicas"`
	HasPDB        bool `json:"has_pdb"`
	Stateful      bool `json:"stateful"`
	MinReplicas   int  `json:"min_replicas"`
}

// MemoryReq asks for a memory recommendation (MiB).
type MemoryReq struct {
	RequestMiB int64   `json:"request_mib"`
	UsageMiB   float64 `json:"usage_mib"`
	MinMiB     int64   `json:"min_mib"`
	MaxMiB     int64   `json:"max_mib"`
}

// ReplicasReq asks for a horizontal recommendation.
type ReplicasReq struct {
	Current int32 `json:"current"`
	Min     int32 `json:"min"`
	Max     int32 `json:"max"`
}

// MemoryDec is the memory recommendation.
type MemoryDec struct {
	Action string `json:"action"`
	MiB    int64  `json:"mib"`
	Reason string `json:"reason"`
}

// ReplicasDec is the horizontal recommendation.
type ReplicasDec struct {
	Target int32  `json:"target"`
	Reason string `json:"reason"`
}

// Request mirrors POST /v1/evaluate.
type Request struct {
	CPUMillicores       int64        `json:"cpu_millicores"`
	P99Ms               float64      `json:"p99_ms"`
	SLOP99Ms            float64      `json:"slo_p99_ms"`
	RPS                 float64      `json:"rps"`
	CPUUsageMillicores  *float64     `json:"cpu_usage_millicores,omitempty"`
	MinMillicores       int64        `json:"min_millicores"`
	MaxMillicores       int64        `json:"max_millicores"`
	MemoryGiB           float64      `json:"memory_gib"`
	CPUPricePerCoreHour float64      `json:"cpu_price_per_core_hour"`
	MemPricePerGiBHour  float64      `json:"mem_price_per_gib_hour"`
	MaxCostPerRequest   float64      `json:"max_cost_per_request"`
	Budget              *Budget      `json:"budget,omitempty"`
	Spot                *Spot        `json:"spot,omitempty"`
	Memory              *MemoryReq   `json:"memory,omitempty"`
	Replicas            *ReplicasReq `json:"replicas,omitempty"`
}

// Decision is the engine's answer.
type Decision struct {
	Action              string       `json:"action"` // shrink | grow | hold
	CPUMillicores       int64        `json:"cpu_millicores"`
	Reason              string       `json:"reason"`
	CostPerRequest      *float64     `json:"cost_per_request"`
	CostPerRequestAfter *float64     `json:"cost_per_request_after"`
	BudgetFrozen        bool         `json:"budget_frozen"`
	SpotEligible        *bool        `json:"spot_eligible"`
	Memory              *MemoryDec   `json:"memory"`
	Replicas            *ReplicasDec `json:"replicas"`
}

// Evaluator evaluates one workload.
type Evaluator interface {
	Evaluate(ctx context.Context, r Request) (Decision, error)
}

// HTTP is an Evaluator over the engine's REST API.
type HTTP struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTP(baseURL string) *HTTP {
	return &HTTP{BaseURL: baseURL, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *HTTP) Evaluate(ctx context.Context, r Request) (Decision, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return Decision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/v1/evaluate", bytes.NewReader(b))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return Decision{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("engine returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var d Decision
	if err := json.Unmarshal(body, &d); err != nil {
		return Decision{}, err
	}
	return d, nil
}
