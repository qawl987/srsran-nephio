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
// +kubebuilder:validation:Enum=eMBB;URLLC;mMTC
type SliceType string

const (
	SliceTypeMBB   SliceType = "eMBB"
	SliceTypeURLLC SliceType = "URLLC"
	SliceTypeMMTC  SliceType = "mMTC"
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
type SliceIntent struct {
	// Type is the slice category: eMBB, URLLC, or mMTC.
	// +kubebuilder:default=eMBB
	Type SliceType `json:"type"`

	// FiveQI is the 5G QoS Identifier (e.g. 7 for eMBB video, 85 for URLLC).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=255
	// +kubebuilder:default=9
	FiveQI int `json:"fiveQI"`

	// MaxLatencyMs is the target per-packet end-to-end latency budget.
	// +kubebuilder:validation:Pattern=`^[0-9]+(ms)?$`
	// +optional
	MaxLatencyMs string `json:"maxLatencyMs,omitempty"`

	// UplinkBandwidth is the aggregate UL bandwidth hint (e.g. "20M").
	// +optional
	UplinkBandwidth string `json:"uplinkBandwidth,omitempty"`

	// DownlinkBandwidth is the aggregate DL bandwidth hint.
	// +optional
	DownlinkBandwidth string `json:"downlinkBandwidth,omitempty"`
}

// TopologySpec describes the UE-side topology of the RAN emulation.
type TopologySpec struct {
	// UECount is the number of simulated UEs to deploy.
	// When 1, a direct DU<->UE ZMQ path is used.
	// When > 1, a RadioBreaker acts as a ZMQ concentrator.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	UECount int `json:"ueCount"`

	// Type overrides the auto-detected topology.
	// +optional
	Type TopologyType `json:"type,omitempty"`

	// PLMN is the Public Land Mobile Network identifier (MCC+MNC, 5 digits).
	// +kubebuilder:default="20893"
	PLMN string `json:"plmn,omitempty"`

	// TAC is the Tracking Area Code that must match the core network.
	// +kubebuilder:default=1
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

// InterfacesSpec declares the network interfaces the operator expects Nephio to populate.
type InterfacesSpec struct {
	// N3 is the gNB-to-UPF (GTP-U) user-plane interface.
	N3 InterfaceIPStatus `json:"n3"`

	// F1 is the F1-AP/F1-U interface between CU-CP/CU-UP and DU.
	F1 InterfaceIPStatus `json:"f1"`

	// SBA is the optional Service-Based Architecture interface.
	// +optional
	SBA *InterfaceIPStatus `json:"sba,omitempty"`
}

// SrsRanDeploymentSpec is the desired state of an SrsRanDeployment.
type SrsRanDeploymentSpec struct {
	// SliceIntent encodes the operator's QoS intent for this RAN slice.
	SliceIntent SliceIntent `json:"sliceIntent"`

	// Topology describes how many UEs should be deployed and in what topology.
	Topology TopologySpec `json:"topology"`

	// Interfaces declares network interfaces whose IPs are injected by Nephio.
	Interfaces InterfacesSpec `json:"interfaces"`

	// DUImage is the container image used for the srsRAN DU / gNB.
	// +kubebuilder:default="docker.io/softwareradiosystems/srsran-project:latest"
	DUImage string `json:"duImage,omitempty"`

	// UEImage is the container image used for srsUE.
	// +kubebuilder:default="docker.io/softwareradiosystems/srsue:latest"
	UEImage string `json:"ueImage,omitempty"`

	// RadioBreakerImage is the container image for the ZMQ proxy / RadioBreaker.
	// +kubebuilder:default="docker.io/softwareradiosystems/radio-breaker:latest"
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
