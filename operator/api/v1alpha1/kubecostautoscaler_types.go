package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types reported in status.conditions.
const (
	ConditionReady            = "Ready"            // spec valid, target found, loop healthy
	ConditionMetricsAvailable = "MetricsAvailable" // cost and demand signals are fresh and valid
	ConditionDegraded         = "Degraded"         // running under the failure policy
	ConditionScalingActive    = "ScalingActive"    // this resource currently owns replica writes
)

// AnnotationPause is the emergency stop. While a KubeCostAutoscaler carries
// autoscaling.kubecost.io/pause="true" the controller keeps evaluating and reporting
// decisions (exactly like spec.dryRun) but writes nothing: no replica changes, no fallback
// HPA created or removed. The workload keeps running at its current size and any native HPA
// may act on it. Removing the annotation resumes control without a restart.
const AnnotationPause = "autoscaling.kubecost.io/pause"

// Failure policy modes.
const (
	FailureModeFreeze                  = "Freeze"
	FailureModeResourceMetricsFallback = "ResourceMetricsFallback"
)

// Scaling owners reported in status.scalingOwner.
const (
	OwnerOperator    = "Operator"
	OwnerFallbackHPA = "FallbackHPA"
	OwnerNone        = "None"
)

// WorkloadRef points at the workload whose replicas are managed. It is always
// in the namespace of the KubeCostAutoscaler: cross-namespace targets are not
// supported.
// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name && self.kind == oldSelf.kind",message="targetRef is immutable"
type WorkloadRef struct {
	// +kubebuilder:validation:Enum=apps/v1
	// +kubebuilder:default="apps/v1"
	APIVersion string `json:"apiVersion,omitempty"`
	// +kubebuilder:validation:Enum=Deployment
	// +kubebuilder:default="Deployment"
	Kind string `json:"kind,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// DemandPolicy defines how resource utilisation drives the replica count.
// +kubebuilder:validation:XValidation:rule="!has(self.targetUtilizationPercent) || !has(self.costPressureUtilizationPercent) || !has(self.ceilingUtilizationPercent) || (self.targetUtilizationPercent <= self.costPressureUtilizationPercent && self.costPressureUtilizationPercent <= self.ceilingUtilizationPercent)",message="require targetUtilizationPercent <= costPressureUtilizationPercent <= ceilingUtilizationPercent"
type DemandPolicy struct {
	// Resources whose utilisation (usage/request, averaged over ready pods) is
	// observed through the resource metrics API; the highest one drives scaling.
	// +kubebuilder:default={"cpu"}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	Resources []DemandResource `json:"resources,omitempty"`
	// TargetUtilizationPercent is the comfortable steady-state utilisation.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetUtilizationPercent int32 `json:"targetUtilizationPercent,omitempty"`
	// CeilingUtilizationPercent is the hard limit: replicas needed to stay at or
	// below it are never withheld for cost reasons.
	// +kubebuilder:default=90
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	CeilingUtilizationPercent int32 `json:"ceilingUtilizationPercent,omitempty"`
	// CostPressureUtilizationPercent is the utilisation the workload may run
	// hot at while its cost exceeds the cost policy.
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	CostPressureUtilizationPercent int32 `json:"costPressureUtilizationPercent,omitempty"`
	// TolerancePercent is the hysteresis band around a utilisation target inside
	// which no change is recommended.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=49
	TolerancePercent *int32 `json:"tolerancePercent,omitempty"`
}

// DemandResource is a resource observable through metrics.k8s.io.
// +kubebuilder:validation:Enum=cpu;memory
type DemandResource string

// CostPolicy bounds the hourly cost of the workload using KubeCost data.
type CostPolicy struct {
	// MaxHourlyCostUSD is the hourly budget (decimal USD, up to 6 decimals).
	// Exceeding it applies cost pressure and, with the guardrail, caps
	// discretionary headroom. It never prevents the replicas needed to stay at
	// or below the utilisation ceiling.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MaxHourlyCostUSD string `json:"maxHourlyCostUSD,omitempty"`
	// TargetHourlyCostUSD is the desired hourly cost; above it the workload is
	// allowed to run hotter (see demand.costPressureUtilizationPercent).
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	TargetHourlyCostUSD string `json:"targetHourlyCostUSD,omitempty"`
	// ScaleUpCostGuardrail caps scale-up headroom at the budget (never below the
	// replicas needed for the utilisation ceiling).
	// +kubebuilder:default=true
	ScaleUpCostGuardrail *bool `json:"scaleUpCostGuardrail,omitempty"`
	// Window is the KubeCost allocation window averaged into an hourly run-rate.
	// +kubebuilder:default="15m"
	Window metav1.Duration `json:"window,omitempty"`
	// Cluster disambiguates the workload in multi-cluster KubeCost installs.
	// +kubebuilder:validation:MaxLength=253
	Cluster string `json:"cluster,omitempty"`
}

// StabilizationPolicy configures anti-flapping. The three controls are
// independent: a window shapes the recommendation, the interval spaces the
// actions, and metric freshness (failurePolicy.maxMetricAge) validates inputs.
type StabilizationPolicy struct {
	// DownscaleWindow: a scale-down only goes as low as the highest
	// recommendation seen during this window.
	// +kubebuilder:default="10m"
	DownscaleWindow metav1.Duration `json:"downscaleWindow,omitempty"`
	// ScaleUpWindow: a (non-safety) scale-up only goes as high as the lowest
	// recommendation seen during this window.
	// +kubebuilder:default="30s"
	ScaleUpWindow metav1.Duration `json:"scaleUpWindow,omitempty"`
	// MinimumScaleActionInterval is the minimum time between two scale actions.
	// Safety scale-ups are exempt.
	// +kubebuilder:default="60s"
	MinimumScaleActionInterval metav1.Duration `json:"minimumScaleActionInterval,omitempty"`
}

// FailurePolicy defines behaviour when signals cannot be trusted.
type FailurePolicy struct {
	// Mode: Freeze keeps the replica count; ResourceMetricsFallback hands
	// scaling to an operator-managed native HPA on CPU after
	// fallbackActivationDelay (only if the resource metrics API works).
	// +kubebuilder:default="Freeze"
	// +kubebuilder:validation:Enum=Freeze;ResourceMetricsFallback
	Mode string `json:"mode,omitempty"`
	// MaxMetricAge is the maximum age of a KubeCost sample.
	// +kubebuilder:default="5m"
	MaxMetricAge metav1.Duration `json:"maxMetricAge,omitempty"`
	// AllowSafetyScaleUp lets the controller add replicas when utilisation
	// exceeds the ceiling even while degraded. Scale-down is never allowed then.
	// +kubebuilder:default=true
	AllowSafetyScaleUp *bool `json:"allowSafetyScaleUp,omitempty"`
	// FallbackActivationDelay is how long cost data must stay invalid before
	// the fallback HPA is created (avoids flapping on short outages).
	// +kubebuilder:default="2m"
	FallbackActivationDelay metav1.Duration `json:"fallbackActivationDelay,omitempty"`
	// RecoveryDelay is how long cost data must stay valid before the fallback
	// HPA is removed and the controller resumes.
	// +kubebuilder:default="2m"
	RecoveryDelay metav1.Duration `json:"recoveryDelay,omitempty"`
	// FallbackTargetCPUUtilizationPercent is the CPU target of the fallback HPA.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	FallbackTargetCPUUtilizationPercent int32 `json:"fallbackTargetCPUUtilizationPercent,omitempty"`
}

// NodeLabelRule matches nodes by label. An empty values list matches any value.
type NodeLabelRule struct {
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
	// +listType=set
	Values []string `json:"values,omitempty"`
}

// InfrastructurePolicy describes Spot/Preemptible vs On-Demand economics.
type InfrastructurePolicy struct {
	// SpotMultiplier is the relative price weight of a Spot replica. Weights
	// only matter relative to each other: the observed blended price is split
	// across the classes present using them. They never imply capacity.
	// +kubebuilder:default="1.0"
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	SpotMultiplier string `json:"spotMultiplier,omitempty"`
	// OnDemandMultiplier is the relative price weight of an On-Demand replica
	// (also used for replicas on nodes of unknown class: conservative).
	// +kubebuilder:default="1.0"
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	OnDemandMultiplier string `json:"onDemandMultiplier,omitempty"`
	// SpotInterruptionBufferPercent discounts the capacity of a Spot replica
	// because it can be reclaimed: it counts as (100-buffer)% of a replica.
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=90
	SpotInterruptionBufferPercent *int32 `json:"spotInterruptionBufferPercent,omitempty"`
	// SpotNodeLabels overrides the built-in Spot label rules when non-empty.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	SpotNodeLabels []NodeLabelRule `json:"spotNodeLabels,omitempty"`
	// OnDemandNodeLabels overrides the built-in On-Demand label rules when non-empty.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	OnDemandNodeLabels []NodeLabelRule `json:"onDemandNodeLabels,omitempty"`
}

// KubeCostAutoscalerSpec defines the desired state.
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= self.minReplicas",message="maxReplicas must be >= minReplicas"
// +kubebuilder:validation:XValidation:rule="!has(self.minHealthyReplicas) || (self.minHealthyReplicas <= self.maxReplicas)",message="minHealthyReplicas must be <= maxReplicas"
type KubeCostAutoscalerSpec struct {
	TargetRef WorkloadRef `json:"targetRef"`
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas"`
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`
	// MinHealthyReplicas is never gone below by a scale-down. Defaults to minReplicas.
	// +kubebuilder:validation:Minimum=0
	MinHealthyReplicas *int32 `json:"minHealthyReplicas,omitempty"`
	// DryRun computes and reports decisions without writing replicas or
	// creating the fallback HPA.
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`
	// EvaluationInterval is how often the workload is re-evaluated.
	// +kubebuilder:default="30s"
	EvaluationInterval metav1.Duration `json:"evaluationInterval,omitempty"`

	// +kubebuilder:default={}
	Demand DemandPolicy `json:"demand,omitempty"`
	// CostPolicy is optional; without it the controller is purely demand-driven.
	// +optional
	CostPolicy *CostPolicy `json:"costPolicy,omitempty"`
	// +kubebuilder:default={}
	Stabilization StabilizationPolicy `json:"stabilization,omitempty"`
	// +kubebuilder:default={}
	FailurePolicy FailurePolicy `json:"failurePolicy,omitempty"`
	// +kubebuilder:default={}
	InfrastructurePolicy InfrastructurePolicy `json:"infrastructurePolicy,omitempty"`
}

// Recommendation is one persisted entry of the stabilisation history.
type Recommendation struct {
	Time     metav1.Time `json:"time"`
	Replicas int32       `json:"replicas"`
}

// DecisionStatus describes the last scaling decision.
type DecisionStatus struct {
	// Type is ScaleUp, ScaleDown, Hold, Fallback or InvalidMetrics.
	Type              string      `json:"type"`
	Reason            string      `json:"reason"`
	Message           string      `json:"message,omitempty"`
	ProposedReplicas  int32       `json:"proposedReplicas"`
	CandidateReplicas int32       `json:"candidateReplicas"`
	CostDriven        bool        `json:"costDriven,omitempty"`
	DryRun            bool        `json:"dryRun,omitempty"`
	Time              metav1.Time `json:"time"`
}

// CostStatus is the last validated cost observation and derived estimates.
type CostStatus struct {
	// HourlyUSD is the allocated run-rate over the sample window (not marginal).
	HourlyUSD string `json:"hourlyUSD"`
	// MarginalAddUSD / MarginalRemoveUSD are the estimated hourly cost of adding / removing one replica.
	MarginalAddUSD    string `json:"marginalAddUSD,omitempty"`
	MarginalRemoveUSD string `json:"marginalRemoveUSD,omitempty"`
	// ProjectedUSD is the estimated hourly cost at the candidate replica count.
	ProjectedUSD string      `json:"projectedUSD,omitempty"`
	SampleTime   metav1.Time `json:"sampleTime"`
	// Stable is false when replicas changed inside the sample window.
	Stable         bool `json:"stable"`
	BudgetExceeded bool `json:"budgetExceeded,omitempty"`
	BudgetLimited  bool `json:"budgetLimited,omitempty"`
}

// InfrastructureStatus counts pods per capacity class.
type InfrastructureStatus struct {
	Spot          int32 `json:"spot"`
	OnDemand      int32 `json:"onDemand"`
	Unknown       int32 `json:"unknown"`
	Pending       int32 `json:"pending"`
	Unschedulable int32 `json:"unschedulable"`
}

// FailureStatus tracks signal health across reconciles (restart-safe).
type FailureStatus struct {
	ConsecutiveFailures int32        `json:"consecutiveFailures"`
	FirstFailureTime    *metav1.Time `json:"firstFailureTime,omitempty"`
	LastHealthyTime     *metav1.Time `json:"lastHealthyTime,omitempty"`
	// HealthySince is when the current run of valid signals started.
	HealthySince *metav1.Time `json:"healthySince,omitempty"`
	// LastErrorClass is a coarse class (never a raw error body).
	LastErrorClass string `json:"lastErrorClass,omitempty"`
}

// FallbackStatus reports the native-HPA fallback.
type FallbackStatus struct {
	Active  bool         `json:"active"`
	Since   *metav1.Time `json:"since,omitempty"`
	HPAName string       `json:"hpaName,omitempty"`
}

// KubeCostAutoscalerStatus defines the observed state.
type KubeCostAutoscalerStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`

	CurrentReplicas int32 `json:"currentReplicas,omitempty"`
	ReadyReplicas   int32 `json:"readyReplicas,omitempty"`
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// Utilization is the observed demand utilisation in percent.
	Utilization string `json:"utilization,omitempty"`

	LastSuccessfulReconcileTime *metav1.Time    `json:"lastSuccessfulReconcileTime,omitempty"`
	LastDecision                *DecisionStatus `json:"lastDecision,omitempty"`
	LastScaleTime               *metav1.Time    `json:"lastScaleTime,omitempty"`
	LastScaleFrom               int32           `json:"lastScaleFrom,omitempty"`
	LastScaleTo                 int32           `json:"lastScaleTo,omitempty"`

	// Recommendations is the persisted stabilisation history (bounded).
	// +kubebuilder:validation:MaxItems=512
	// +listType=atomic
	Recommendations []Recommendation `json:"recommendations,omitempty"`

	Cost           *CostStatus          `json:"cost,omitempty"`
	Infrastructure InfrastructureStatus `json:"infrastructure,omitempty"`
	Failure        FailureStatus        `json:"failure,omitempty"`
	Fallback       FallbackStatus       `json:"fallback,omitempty"`

	// ScalingOwner is who writes spec.replicas right now: Operator, FallbackHPA or None.
	ScalingOwner string `json:"scalingOwner,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kca
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Min",type=integer,JSONPath=`.spec.minReplicas`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxReplicas`
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="$/h",type=string,JSONPath=`.status.cost.hourlyUSD`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.status.lastDecision.type`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.status.scalingOwner`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// KubeCostAutoscaler scales a Deployment's replicas using resource demand as the
// driver and KubeCost cost data as a bounded optimisation signal.
type KubeCostAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubeCostAutoscalerSpec   `json:"spec,omitempty"`
	Status KubeCostAutoscalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KubeCostAutoscalerList contains a list of KubeCostAutoscaler.
type KubeCostAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubeCostAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KubeCostAutoscaler{}, &KubeCostAutoscalerList{})
}
