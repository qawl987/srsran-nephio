// Copyright 2024 The Nephio Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SliceType defines the RAN slice profile.
// +kubebuilder:validation:Enum=eMBB;URLLC
type SliceType string

const (
	SliceTypeMBB   SliceType = "eMBB"
	SliceTypeURLLC SliceType = "URLLC"
)

// TopologyType distinguishes single-UE from multi-UE RadioBreaker scenarios.
// +kubebuilder:validation:Enum=single;multi
type TopologyType string

const (
	TopologySingle TopologyType = "single"
	TopologyMulti  TopologyType = "multi"
)

// SrsRanDeploymentConditionType is a type for SrsRanDeployment conditions.
type SrsRanDeploymentConditionType string

const (
	ConditionTypeReady       SrsRanDeploymentConditionType = "Ready"
	ConditionTypeReconciling SrsRanDeploymentConditionType = "Reconciling"
	ConditionTypeError       SrsRanDeploymentConditionType = "Error"
)

// SliceIntent captures the high-level network slice QoS intent for the RAN.
//
// Supported 5QI values:
//
//	eMBB  : 9 (default best-effort), 7 (real-time video), 10 (low-latency video)
//	URLLC : 82 (mission-critical data), 84 (V2X messages), 85 (mission-critical MPS)
type SliceIntent struct {
	// Type is the slice category: eMBB or URLLC.
	// +kubebuilder:default=eMBB
	Type SliceType `json:"type"`

	// FiveQI is the 5G QoS Identifier.
	// eMBB  valid values: 7, 9, 10
	// URLLC valid values: 82, 84, 85
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=255
	// +kubebuilder:default=9
	FiveQI int `json:"fiveQI"`

	// MaxLatencyMs is the target per-packet end-to-end latency budget (e.g. "10ms").
	// +optional
	MaxLatencyMs string `json:"maxLatencyMs,omitempty"`

	// UplinkBandwidth is the aggregate UL bandwidth hint (e.g. "20M").
	// +optional
	UplinkBandwidth string `json:"uplinkBandwidth,omitempty"`

	// DownlinkBandwidth is the aggregate DL bandwidth hint.
	// +optional
	DownlinkBandwidth string `json:"downlinkBandwidth,omitempty"`
}

// TopologySpec describes the UE-side ZMQ simulation topology.
type TopologySpec struct {
	// UECount is the number of simulated UEs to deploy.
	// When 1: single-topology – DU ZMQ connects directly to the UE Service IP.
	// When >1: multi-topology – a RadioBreaker ZMQ proxy is injected;
	//   DU connects to the RadioBreaker, each UE gets a unique ZMQ port pair
	//   starting at base port 2000 and incrementing by 2 (2000, 2002, 2004...).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	UECount int `json:"ueCount"`

	// Type overrides auto-detected topology. Leave empty to auto-detect from UECount.
	// +optional
	Type TopologyType `json:"type,omitempty"`

	// PLMN is the Public Land Mobile Network identifier (MCC+MNC, 5 digits).
	// +kubebuilder:default="20893"
	// +optional
	PLMN string `json:"plmn,omitempty"`

	// TAC is the Tracking Area Code that must match the free5GC core configuration.
	// +kubebuilder:default=1
	// +optional
	TAC int `json:"tac,omitempty"`
}

// InterfaceIPStatus holds the IP address injected by the Nephio IPAM function.
type InterfaceIPStatus struct {
	// Name is the logical interface name (e.g. "n3", "f1").
	Name string `json:"name"`

	// IPAddress is the CIDR-formatted IP injected by the Nephio ipam-fn.
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`

	// Gateway is the default gateway for this interface.
	// +optional
	Gateway string `json:"gateway,omitempty"`
}

// InterfacesSpec declares the five 5G NR interfaces the operator expects the
// Nephio IPAM pipeline to populate before reconciliation begins.
//
//	Interface  Plane    Endpoints
//	─────────  ───────  ──────────────────────────────
//	N2         Control  CU-CP  ↔ AMF  (NGAP over SCTP)
//	N3         User     CU-UP  ↔ UPF  (GTP-U)
//	E1         Control  CU-CP  ↔ CU-UP (E1AP)
//	F1C        Control  CU-CP  ↔ DU   (F1-AP)
//	F1U        User     CU-UP  ↔ DU   (GTP-U / F1-U)
//
// IMPORTANT: Because CU-CP, CU-UP, and DU run in SEPARATE pods, the operator
// must use these injected Service IPs – loopback addresses will not work.
type InterfacesSpec struct {
	// N2 is the CU-CP to AMF control-plane interface (NGAP).
	N2 InterfaceIPStatus `json:"n2"`

	// N3 is the CU-UP to UPF user-plane interface (GTP-U).
	N3 InterfaceIPStatus `json:"n3"`

	// E1 is the inter-CU interface between CU-CP and CU-UP (E1AP).
	E1 InterfaceIPStatus `json:"e1"`

	// F1C is the F1 control-plane interface from CU-CP to DU (F1-AP).
	F1C InterfaceIPStatus `json:"f1c"`

	// F1U is the F1 user-plane interface from CU-UP to DU (GTP-U).
	F1U InterfaceIPStatus `json:"f1u"`
}

// SrsRanDeploymentSpec is the desired state of an SrsRanDeployment.
// The operator deploys THREE completely separate pods: CU-CP, CU-UP, and DU.
// All cross-pod communication uses Kubernetes Service IPs injected by Nephio IPAM.
type SrsRanDeploymentSpec struct {
	// SliceIntent encodes the E2E RAN/core slice QoS intent.
	SliceIntent SliceIntent `json:"sliceIntent"`

	// Topology describes the UE-side ZMQ simulation topology.
	Topology TopologySpec `json:"topology"`

	// Interfaces declares the five 5G NR interfaces whose IPs are injected by
	// the Nephio interface-fn KRM function at package specialization time.
	// Fields are intentionally empty in the blueprint.
	Interfaces InterfacesSpec `json:"interfaces"`

	// CUCPImage is the container image for the srsRAN CU-CP process.
	// +kubebuilder:default="docker.io/softwareradiosystems/srsran-project:latest"
	// +optional
	CUCPImage string `json:"cucpImage,omitempty"`

	// CUUPImage is the container image for the srsRAN CU-UP process.
	// +kubebuilder:default="docker.io/softwareradiosystems/srsran-project:latest"
	// +optional
	CUUPImage string `json:"cuupImage,omitempty"`

	// DUImage is the container image for the srsRAN DU process.
	// +kubebuilder:default="docker.io/softwareradiosystems/srsran-project:latest"
	// +optional
	DUImage string `json:"duImage,omitempty"`

	// UEImage is the container image for the srsUE process.
	// +kubebuilder:default="docker.io/softwareradiosystems/srsue:latest"
	// +optional
	UEImage string `json:"ueImage,omitempty"`

	// RadioBreakerImage is the ZMQ proxy image used for multi-UE topology.
	// +kubebuilder:default="docker.io/softwareradiosystems/radio-breaker:latest"
	// +optional
	RadioBreakerImage string `json:"radioBreakerImage,omitempty"`
}

// SrsRanDeploymentStatus is the observed state of an SrsRanDeployment.
type SrsRanDeploymentStatus struct {
	// Conditions describe the current operational state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation last successfully reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ActiveTopology records whether "single" or "multi" topology is live.
	// +optional
	ActiveTopology TopologyType `json:"activeTopology,omitempty"`

	// ReadyUEs is the count of UE pods in the Ready state.
	// +optional
	ReadyUEs int32 `json:"readyUEs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=srsran
// +kubebuilder:printcolumn:name="Slice",type=string,JSONPath=`.spec.sliceIntent.type`
// +kubebuilder:printcolumn:name="5QI",type=integer,JSONPath=`.spec.sliceIntent.fiveQI`
// +kubebuilder:printcolumn:name="UEs",type=integer,JSONPath=`.spec.topology.ueCount`
// +kubebuilder:printcolumn:name="Topology",type=string,JSONPath=`.status.activeTopology`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SrsRanDeployment is the Schema for the srsrandeployments API.
type SrsRanDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SrsRanDeploymentSpec   `json:"spec,omitempty"`
	Status SrsRanDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SrsRanDeploymentList contains a list of SrsRanDeployment.
type SrsRanDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SrsRanDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SrsRanDeployment{}, &SrsRanDeploymentList{})
}
