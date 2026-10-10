package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TargetRef points at the workload being scaled. Only Deployments are supported.
type TargetRef struct {
	// +kubebuilder:validation:Enum=Deployment
	// +kubebuilder:default=Deployment
	Kind string `json:"kind,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// ContainerName is the container to resize. Defaults to the first container.
	ContainerName string `json:"containerName,omitempty"`
}

// SLO is the latency objective used to decide whether resources can shrink.
type SLO struct {
	// P99LatencyMs is the commercial p99 latency target in milliseconds.
	// +kubebuilder:validation:Minimum=1
	P99LatencyMs int32 `json:"p99LatencyMs"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || quantity(self.min).compareTo(quantity(self.max)) <= 0",message="cpu.min must be <= cpu.max"

// CPUBounds limits how far the operator may resize CPU requests.
type CPUBounds struct {
	// +kubebuilder:default="100m"
	Min resource.Quantity `json:"min,omitempty"`
	// +kubebuilder:default="4"
	Max resource.Quantity `json:"max,omitempty"`
}

// UnitEconomics configures cost-per-request evaluation.
type UnitEconomics struct {
	// CPUPricePerCoreHour in USD. Falls back to the operator default when zero.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	CPUPricePerCoreHour string `json:"cpuPricePerCoreHour,omitempty"`
	// MemPricePerGiBHour in USD. Falls back to the operator default when zero.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MemPricePerGiBHour string `json:"memPricePerGiBHour,omitempty"`
	// MaxCostPerRequest in USD. When exceeded and the SLO has slack, the pod shrinks.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MaxCostPerRequest string `json:"maxCostPerRequest,omitempty"`
}

// Budget freezes growth when the monthly spend of the namespace reaches the limit.
type Budget struct {
	// MonthlyLimitUSD is the namespace monthly budget in USD.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MonthlyLimitUSD string `json:"monthlyLimitUSD"`
	// OnSpendUnavailable decides what happens when KubeCost cannot be reached.
	// Ignore keeps serving the SLO (budget not enforced that cycle); FreezeGrowth
	// treats the budget as exhausted, blocking growth until spend is known again.
	// +kubebuilder:validation:Enum=Ignore;FreezeGrowth
	// +kubebuilder:default=Ignore
	OnSpendUnavailable string `json:"onSpendUnavailable,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || quantity(self.min).compareTo(quantity(self.max)) <= 0",message="memory.min must be <= memory.max"

// MemorySpec opts the workload in to memory resizing (grow fast, shrink slowly).
type MemorySpec struct {
	// +kubebuilder:default="64Mi"
	Min resource.Quantity `json:"min,omitempty"`
	// +kubebuilder:default="16Gi"
	Max resource.Quantity `json:"max,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.max >= self.min",message="replicas.max must be >= replicas.min"

// ReplicaSpec opts the workload in to horizontal scaling once vertical room is
// exhausted. Do not combine with another HPA targeting the same Deployment.
type ReplicaSpec struct {
	// +kubebuilder:validation:Minimum=1
	Min int32 `json:"min"`
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`
}

// SpotSpec controls Spot awareness for the workload.
type SpotSpec struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
	// FaultTolerant marks the workload as safe to run on interruptible capacity.
	FaultTolerant bool `json:"faultTolerant,omitempty"`
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`
}

// KubeCostHPASpec defines the desired state of KubeCostHPA.
type KubeCostHPASpec struct {
	TargetRef TargetRef `json:"targetRef"`
	SLO       SLO       `json:"slo"`
	// +optional
	// +kubebuilder:default={}
	CPU CPUBounds `json:"cpu,omitempty"`
	// +optional
	UnitEconomics UnitEconomics `json:"unitEconomics,omitempty"`
	// +optional
	Budget *Budget `json:"budget,omitempty"`
	// +optional
	// +kubebuilder:default={}
	Spot SpotSpec `json:"spot,omitempty"`
	// Memory enables memory resizing when set.
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`
	// Replicas enables horizontal scaling when set.
	// +optional
	Replicas *ReplicaSpec `json:"replicas,omitempty"`
	// Cooldown is the minimum time between two resize actions.
	// +kubebuilder:default="5m"
	Cooldown metav1.Duration `json:"cooldown,omitempty"`
	// Interval is how often the workload is re-evaluated.
	// +kubebuilder:default="1m"
	Interval metav1.Duration `json:"interval,omitempty"`
	// DryRun computes and reports decisions without touching pods.
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`
}

// KubeCostHPAStatus defines the observed state of KubeCostHPA.
type KubeCostHPAStatus struct {
	// DesiredCPUMillicores is the CPU request the operator converges pods to.
	DesiredCPUMillicores int64  `json:"desiredCPUMillicores,omitempty"`
	DesiredMemoryMiB     int64  `json:"desiredMemoryMiB,omitempty"`
	DesiredReplicas      int32  `json:"desiredReplicas,omitempty"`
	LastAction           string `json:"lastAction,omitempty"`
	// LastResizeAction is "grow" or "shrink" for the last applied change (drives the asymmetric cooldown).
	LastResizeAction string `json:"lastResizeAction,omitempty"`
	LastReason       string `json:"lastReason,omitempty"`
	RPS              string `json:"rps,omitempty"`
	P99LatencyMs     string `json:"p99LatencyMs,omitempty"`
	CostPerRequest   string `json:"costPerRequest,omitempty"`
	SpotEligible     bool   `json:"spotEligible,omitempty"`
	BudgetFrozen     bool   `json:"budgetFrozen,omitempty"`
	// +optional
	LastResizeTime *metav1.Time `json:"lastResizeTime,omitempty"`
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kchpa
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="CPU(m)",type=integer,JSONPath=`.status.desiredCPUMillicores`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.status.lastAction`
// +kubebuilder:printcolumn:name="$/req",type=string,JSONPath=`.status.costPerRequest`
// +kubebuilder:printcolumn:name="Frozen",type=boolean,JSONPath=`.status.budgetFrozen`

// KubeCostHPA is the Schema for the kubecosthpas API.
type KubeCostHPA struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubeCostHPASpec   `json:"spec,omitempty"`
	Status KubeCostHPAStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KubeCostHPAList contains a list of KubeCostHPA.
type KubeCostHPAList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubeCostHPA `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KubeCostHPA{}, &KubeCostHPAList{})
}
