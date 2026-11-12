package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phases of a WorkloadRecommendation.
const (
	RecPhasePending    = "Pending"    // published, waiting for a human approval
	RecPhaseDryRun     = "DryRun"     // approved and allowed, but the operator is in observe-only mode: nothing was written
	RecPhaseApplying   = "Applying"   // the change is being applied or watched
	RecPhaseVerified   = "Verified"   // applied and healthy for the whole observation window
	RecPhaseRolledBack = "RolledBack" // applied, degraded, and restored
	RecPhaseDenied     = "Denied"     // the policy refused it (see status.reasons)
	RecPhaseFailed     = "Failed"     // an error the operator could not recover from; a human must look
)

// WorkloadRecommendationSpec is a recommendation produced by the read-only recommender (rightsize plan --publish).
// Creating this object changes nothing in the cluster. Only an explicit approval (status.approval) plus the operator's
// applier policy can cause a change, and only to a workload in the SAME namespace as this object.
type WorkloadRecommendationSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	RecommendationID string               `json:"recommendationID"`
	Target           RecommendationTarget `json:"target"`
	// Record is the full recommendation as JSON, exactly as the recommender produced it, so it can be reproduced and audited.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=262144
	Record string `json:"record"`
	// Summary is a human-readable digest for kubectl; it is NOT what is applied (Record is).
	// +optional
	Summary RecommendationSummary `json:"summary,omitempty"`
	// SLOQueryName selects one of the named SLO queries in the operator's policy ConfigMap (key "slo.<name>"). Free-form PromQL is not accepted.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	SLOQueryName string `json:"sloQueryName,omitempty"`
}

// RecommendationTarget names the workload and container.
type RecommendationTarget struct {
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Container string `json:"container"`
}

// RecommendationSummary is for display only.
type RecommendationSummary struct {
	CPU        string `json:"cpu,omitempty"`
	Memory     string `json:"memory,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	// PotentialMonthlyUSD is an ESTIMATE of the requests reduction times supplied unit prices. It is not a bill and not an observed saving.
	PotentialMonthlyUSD string `json:"potentialMonthlyUSD,omitempty"`
}

// RecommendationApproval is written through the status subresource, so approving needs its own RBAC permission
// (update workloadrecommendations/status) separate from creating recommendations.
type RecommendationApproval struct {
	// RecommendationID must equal spec.recommendationID: an approval of another recommendation is ignored.
	RecommendationID string `json:"recommendationID"`
	// By is the username the API server reported for the approver (SelfSubjectReview), not a typed name.
	By string      `json:"by"`
	At metav1.Time `json:"at"`
}

// WorkloadRecommendationStatus is owned by the operator and the approvers.
type WorkloadRecommendationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=Pending;DryRun;Applying;Verified;RolledBack;Denied;Failed
	Phase   string   `json:"phase,omitempty"`
	Message string   `json:"message,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	// +optional
	Approval *RecommendationApproval `json:"approval,omitempty"`
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wrec
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=`.spec.summary.cpu`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.summary.memory`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Approved by",type=string,JSONPath=`.status.approval.by`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkloadRecommendation is a published, auditable rightsizing recommendation.
type WorkloadRecommendation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadRecommendationSpec   `json:"spec,omitempty"`
	Status WorkloadRecommendationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadRecommendationList is a list of WorkloadRecommendation.
type WorkloadRecommendationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadRecommendation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadRecommendation{}, &WorkloadRecommendationList{})
}
