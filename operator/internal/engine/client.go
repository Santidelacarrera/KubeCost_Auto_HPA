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

// PredictiveReq configures proactive scaling ahead of traffic surges (ScaleOps parity).
type PredictiveReq struct {
	WarmupLeadTimeSeconds int32     `json:"warmup_lead_time_seconds,omitempty"`
	TargetRPSPerPod       float64   `json:"target_rps_per_pod,omitempty"`
	RPSHistory            []float64 `json:"rps_history,omitempty"`
	HourOfDayUTC          *int      `json:"hour_of_day_utc,omitempty"`
}

// PredictiveDec is the engine's predictive forecast output.
type PredictiveDec struct {
	PredictedReplicas   int32   `json:"predicted_replicas"`
	ReactiveReplicas    int32   `json:"reactive_replicas"`
	RecommendedReplicas int32   `json:"recommended_replicas"`
	IsPrewarming        bool    `json:"is_prewarming"`
	ConfidenceScore     float64 `json:"confidence_score"`
	Reason              string  `json:"reason"`
}

// OOMProtectionReq monitors heap accumulation to prevent 137 exit codes (ScaleOps parity).
type OOMProtectionReq struct {
	CriticalThreshold float64   `json:"critical_threshold,omitempty"`
	WarningThreshold  float64   `json:"warning_threshold,omitempty"`
	UsageHistoryMiB   []float64 `json:"usage_history_mib,omitempty"`
	MaxLimitMiB       int64     `json:"max_allowed_limit_mib,omitempty"`
}

// OOMProtectionDec is the OOM shield intervention output.
type OOMProtectionDec struct {
	IsAtRisk              bool     `json:"is_at_risk"`
	RiskLevel             string   `json:"risk_level"`
	RecommendedLimitMiB   int64    `json:"recommended_limit_mib"`
	RecommendedRequestMiB int64    `json:"recommended_request_mib"`
	HeadroomPercentage    float64  `json:"headroom_percentage"`
	EstimatedSecondsToOOM *float64 `json:"estimated_seconds_to_oom"`
	Action                string   `json:"action"`
	Reason                string   `json:"reason"`
}

// Request mirrors POST /v1/evaluate.
type Request struct {
	CPUMillicores       int64             `json:"cpu_millicores"`
	P99Ms               float64           `json:"p99_ms"`
	SLOP99Ms            float64           `json:"slo_p99_ms"`
	RPS                 float64           `json:"rps"`
	CPUUsageMillicores  *float64          `json:"cpu_usage_millicores,omitempty"`
	MinMillicores       int64             `json:"min_millicores"`
	MaxMillicores       int64             `json:"max_millicores"`
	MemoryGiB           float64           `json:"memory_gib"`
	CPUPricePerCoreHour float64           `json:"cpu_price_per_core_hour"`
	MemPricePerGiBHour  float64           `json:"mem_price_per_gib_hour"`
	MaxCostPerRequest   float64           `json:"max_cost_per_request"`
	Budget              *Budget           `json:"budget,omitempty"`
	Spot                *Spot             `json:"spot,omitempty"`
	Memory              *MemoryReq        `json:"memory,omitempty"`
	Replicas            *ReplicasReq      `json:"replicas,omitempty"`
	Predictive          *PredictiveReq    `json:"predictive,omitempty"`
	OOMProtection       *OOMProtectionReq `json:"oom_protection,omitempty"`
}

// Decision is the engine's answer.
type Decision struct {
	Action              string            `json:"action"` // shrink | grow | hold
	CPUMillicores       int64             `json:"cpu_millicores"`
	Reason              string            `json:"reason"`
	CostPerRequest      *float64          `json:"cost_per_request"`
	CostPerRequestAfter *float64          `json:"cost_per_request_after"`
	BudgetFrozen        bool              `json:"budget_frozen"`
	SpotEligible        *bool             `json:"spot_eligible"`
	Memory              *MemoryDec        `json:"memory"`
	Replicas            *ReplicasDec      `json:"replicas"`
	Predictive          *PredictiveDec    `json:"predictive,omitempty"`
	OOMProtection       *OOMProtectionDec `json:"oom_protection,omitempty"`
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

// TelemetryMetrics represents the backward-compatible Section 13.1 metrics payload.
type TelemetryMetrics struct {
	RPS             float64 `json:"rps"`
	P99LatencyMs    float64 `json:"p99_latency_ms"`
	CPUUsageCores   float64 `json:"cpu_usage_cores"`
	MemUsageBytes   int64   `json:"mem_usage_bytes"`
	CurrentReplicas int32   `json:"current_replicas"`
}

// TelemetryPayload supports both Section 13.1 legacy fields and Section 13.2 multi-level fields.
type TelemetryPayload struct {
	SchemaVersion      string           `json:"schema_version,omitempty"`
	EventID            string           `json:"event_id,omitempty"`
	ClientID           string           `json:"client_id"`
	ClusterID          string           `json:"cluster_id"`
	AgentVersion       string           `json:"agent_version,omitempty"`
	ClusterVersion     string           `json:"cluster_version,omitempty"`
	NodeName           string           `json:"node_name,omitempty"`
	NodeInstanceType   string           `json:"node_instance_type,omitempty"`
	SpotInstanceRisk   float64          `json:"spot_instance_risk,omitempty"`
	Namespace          string           `json:"namespace,omitempty"`
	WorkloadName       string           `json:"workload_name,omitempty"`
	WorkloadKind       string           `json:"workload_kind,omitempty"`
	PodName            string           `json:"pod_name,omitempty"`
	ContainerName      string           `json:"container_name,omitempty"`
	CPURequests        string           `json:"cpu_requests,omitempty"`
	CPULimits          string           `json:"cpu_limits,omitempty"`
	MemoryRequests     string           `json:"memory_requests,omitempty"`
	MemoryLimits       string           `json:"memory_limits,omitempty"`
	SuccessfulRequests float64          `json:"successful_requests,omitempty"`
	ErrorRate          float64          `json:"error_rate,omitempty"`
	P50LatencyMs       float64          `json:"p50_latency_ms,omitempty"`
	P95LatencyMs       float64          `json:"p95_latency_ms,omitempty"`
	Metrics            TelemetryMetrics `json:"metrics"`
	Timestamp          int64            `json:"timestamp"`
}

// PreWarmCommand mirrors Section 7.5 PRE_WARM command contract from the SaaS control plane.
type PreWarmCommand struct {
	CommandID           string            `json:"command_id"`
	ClientID            string            `json:"client_id"`
	ClusterID           string            `json:"cluster_id"`
	Workload            string            `json:"workload"`
	Action              string            `json:"action"`
	RequestedResources  map[string]string `json:"requested_resources"`
	RecommendedReplicas int32             `json:"recommended_replicas"`
	ExecuteAt           int64             `json:"execute_at"`
	ExpiresAt           int64             `json:"expires_at"`
	Confidence          float64           `json:"confidence"`
	Reason              string            `json:"reason"`
	PolicyVersion       string            `json:"policy_version"`
}

// CommandACK reports command execution or rejection back to the SaaS control plane.
type CommandACK struct {
	ClientID  string `json:"client_id"`
	CommandID string `json:"command_id"`
	Status    string `json:"status"` // EXECUTED | REJECTED_EXPIRED | REJECTED_CAPACITY | REJECTED_GUARDRAIL | DUPLICATE_IGNORED
	Reason    string `json:"reason"`
	Timestamp int64  `json:"timestamp"`
}

// IngestTelemetry sends multi-level telemetry to POST /v1/telemetry/ingest.
func (h *HTTP) IngestTelemetry(ctx context.Context, payload TelemetryPayload) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/v1/telemetry/ingest", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if payload.ClientID != "" {
		req.Header.Set("X-Tenant-ID", payload.ClientID)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("telemetry ingest returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// ValidatePreWarmCommand validates a PRE_WARM command locally before execution,
// enforcing anti-replay, expiration, confidence floor, and cluster capacity checks.
func ValidatePreWarmCommand(
	cmd PreWarmCommand,
	nowUnix int64,
	executedIDs map[string]bool,
	minConfidence float64,
	clusterHasCapacity bool,
	maxAllowedReplicas int32,
) CommandACK {
	if executedIDs != nil && executedIDs[cmd.CommandID] {
		return CommandACK{
			ClientID:  cmd.ClientID,
			CommandID: cmd.CommandID,
			Status:    "DUPLICATE_IGNORED",
			Reason:    "command_id already executed locally (anti-replay)",
			Timestamp: nowUnix,
		}
	}
	if cmd.ExpiresAt <= nowUnix {
		return CommandACK{
			ClientID:  cmd.ClientID,
			CommandID: cmd.CommandID,
			Status:    "REJECTED_EXPIRED",
			Reason:    fmt.Sprintf("command expired at %d (now=%d)", cmd.ExpiresAt, nowUnix),
			Timestamp: nowUnix,
		}
	}
	if cmd.Confidence < minConfidence {
		return CommandACK{
			ClientID:  cmd.ClientID,
			CommandID: cmd.CommandID,
			Status:    "REJECTED_GUARDRAIL",
			Reason:    fmt.Sprintf("confidence %.2f below minimum threshold %.2f", cmd.Confidence, minConfidence),
			Timestamp: nowUnix,
		}
	}
	if !clusterHasCapacity {
		return CommandACK{
			ClientID:  cmd.ClientID,
			CommandID: cmd.CommandID,
			Status:    "REJECTED_CAPACITY",
			Reason:    "cluster lacks allocatable capacity for PRE_WARM expansion",
			Timestamp: nowUnix,
		}
	}
	if maxAllowedReplicas > 0 && cmd.RecommendedReplicas > maxAllowedReplicas {
		return CommandACK{
			ClientID:  cmd.ClientID,
			CommandID: cmd.CommandID,
			Status:    "REJECTED_GUARDRAIL",
			Reason:    fmt.Sprintf("recommended_replicas %d exceeds local guardrail max %d", cmd.RecommendedReplicas, maxAllowedReplicas),
			Timestamp: nowUnix,
		}
	}
	if executedIDs != nil {
		executedIDs[cmd.CommandID] = true
	}
	return CommandACK{
		ClientID:  cmd.ClientID,
		CommandID: cmd.CommandID,
		Status:    "EXECUTED",
		Reason:    "PRE_WARM command validated and applied within local guardrails",
		Timestamp: nowUnix,
	}
}
