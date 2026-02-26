/*
Copyright 2024 The Nephio Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controllers implements the SrsRanDeployment reconciler.
//
// Architecture: three SEPARATE pods are deployed per SrsRanDeployment:
//
//	CU-CP  – binds N2 (NGAP→AMF), E1 (E1AP→CU-UP), F1C (F1-AP→DU)
//	CU-UP  – binds N3 (GTP-U→UPF), E1 (E1AP←CU-CP), F1U (GTP-U→DU)
//	DU     – binds F1C (F1-AP←CU-CP), F1U (GTP-U←CU-UP), ZMQ RF
//
// Because the three components run in separate pods, ALL inter-component
// addresses come from Nephio-injected IPAM IPs exposed via Kubernetes Services.
// Loopback (127.x.x.x) addresses must never appear in generated configs.
package controllers

import (
	"context"
	"fmt"
	"net"
	"time"

	srsranov1alpha1 "github.com/nephio-project/srsran-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ConfigMapVersionAnnotation is applied to pod templates to trigger rolling
	// restarts when the underlying ConfigMap changes.
	ConfigMapVersionAnnotation = "workload.nephio.org/configMapVersion"

	// zmqBasePort is the first ZMQ TX port allocated to the first UE (or the
	// single UE in single topology). Each additional UE gets +2 (TX/RX pair).
	zmqBasePort = 2000

	// requeuAfterSeconds is used for requeuing on transient conditions.
	requeueAfterSeconds = 10
)

// SrsRanDeploymentReconciler reconciles an SrsRanDeployment object.
type SrsRanDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the Manager.
// It owns Deployments and ConfigMaps so that changes to owned resources
// trigger reconciliation of the parent SrsRanDeployment.
func (r *SrsRanDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(new(srsranov1alpha1.SrsRanDeployment)).
		Owns(new(appsv1.Deployment)).
		Owns(new(apiv1.ConfigMap)).
		Complete(r)
}

// +kubebuilder:rbac:groups=workload.nephio.org,resources=srsrandeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workload.nephio.org,resources=srsrandeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workload.nephio.org,resources=srsrandeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is the main control loop.
//
// High-level flow:
//  1. Fetch the SrsRanDeployment CR.
//  2. Validate all 5 Nephio IPAM-injected IPs are present.
//  3. Reconcile QoS ConfigMap (DU-mounted, based on 5QI).
//  4. Reconcile CU-CP  ConfigMap + Deployment  (N2, E1, F1C bindings).
//  5. Reconcile CU-UP  ConfigMap + Deployment  (N3, E1, F1U bindings).
//  6. Reconcile DU     ConfigMap + Deployment  (F1C, F1U, ZMQ RF bindings).
//  7. Reconcile ZMQ UE topology (single or RadioBreaker multi).
//  8. Update status.
func (r *SrsRanDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("SrsRanDeployment", req.NamespacedName)

	// ── 1. Fetch CR ──────────────────────────────────────────────────────────
	cr := new(srsranov1alpha1.SrsRanDeployment)
	if err := r.Client.Get(ctx, req.NamespacedName, cr); err != nil {
		if k8serrors.IsNotFound(err) {
			log.Info("SrsRanDeployment not found; object was deleted")
			return reconcile.Result{}, nil
		}
		log.Error(err, "Failed to get SrsRanDeployment")
		return reconcile.Result{}, err
	}

	// ── 2. Validate all Nephio IPAM-injected IPs ─────────────────────────────
	// The Nephio pipeline (interface-fn) must inject real IPs into all five
	// interface fields before the operator can generate valid configs.
	// CU-CP, CU-UP, and DU run in SEPARATE pods; cross-pod addresses must be
	// real routed IPs – loopback 127.x.x.x will not work.
	type ifCheck struct{ name, raw string }
	for _, ifc := range []ifCheck{
		{"n2", cr.Spec.Interfaces.N2.IPAddress},
		{"n3", cr.Spec.Interfaces.N3.IPAddress},
		{"e1", cr.Spec.Interfaces.E1.IPAddress},
		{"f1c", cr.Spec.Interfaces.F1C.IPAddress},
		{"f1u", cr.Spec.Interfaces.F1U.IPAddress},
	} {
		if _, err := extractHost(ifc.raw); err != nil {
			log.Info("Interface IP not yet injected by Nephio IPAM; requeueing",
				"interface", ifc.name, "raw", ifc.raw)
			return reconcile.Result{RequeueAfter: time.Duration(requeueAfterSeconds) * time.Second}, nil
		}
	}

	n2IP, _ := extractHost(cr.Spec.Interfaces.N2.IPAddress)
	n3IP, _ := extractHost(cr.Spec.Interfaces.N3.IPAddress)
	e1IP, _ := extractHost(cr.Spec.Interfaces.E1.IPAddress)
	f1cIP, _ := extractHost(cr.Spec.Interfaces.F1C.IPAddress)
	f1uIP, _ := extractHost(cr.Spec.Interfaces.F1U.IPAddress)

	// ── 3. Determine effective ZMQ topology ──────────────────────────────────
	topology := effectiveTopology(cr)
	log.Info("Resolved topology", "type", topology, "ueCount", cr.Spec.Topology.UECount)

	// ── 4. QoS ConfigMap (mounted into DU) ───────────────────────────────────
	qosData, err := renderQoSConfigMap(cr.Spec.SliceIntent)
	if err != nil {
		log.Error(err, "Failed to render QoS ConfigMap")
		return reconcile.Result{}, err
	}
	qosCMName := cr.Name + "-qos"
	if err := r.reconcileConfigMap(ctx, cr,
		buildConfigMap(qosCMName, cr.Namespace, map[string]string{"qos.yaml": qosData}),
		log); err != nil {
		return reconcile.Result{}, err
	}

	// ── 5. CU-CP ConfigMap + Deployment ──────────────────────────────────────
	// CU-CP binds: N2 (NGAP→AMF), E1 (E1AP server), F1C (F1-AP server).
	cucpCfgData, err := renderCUCPConfig(CUCPConfigValues{
		N2BindAddr:  n2IP,
		AMFAddr:     cr.Spec.Interfaces.N2.Gateway, // Gateway = AMF IP hint
		E1BindAddr:  e1IP,
		F1CBindAddr: f1cIP,
		PLMN:        cr.Spec.Topology.PLMN,
		TAC:         cr.Spec.Topology.TAC,
	})
	if err != nil {
		log.Error(err, "Failed to render CU-CP config")
		return reconcile.Result{}, err
	}
	cucpCMName := cr.Name + "-cucp"
	if err := r.reconcileConfigMap(ctx, cr,
		buildConfigMap(cucpCMName, cr.Namespace, map[string]string{"cu_cp.yml": cucpCfgData}),
		log); err != nil {
		return reconcile.Result{}, err
	}
	cucpCM := new(apiv1.ConfigMap)
	_ = r.Client.Get(ctx, types.NamespacedName{Name: cucpCMName, Namespace: cr.Namespace}, cucpCM)
	if err := r.reconcileDeployment(ctx, cr,
		buildCUCPDeployment(cr, cucpCMName, cucpCM.ResourceVersion), log); err != nil {
		return reconcile.Result{}, err
	}
	// CU-CP Service: exposes E1AP and F1-AP so CU-UP and DU can reach it.
	if err := r.reconcileService(ctx, cr, buildCUCPService(cr), log); err != nil {
		return reconcile.Result{}, err
	}

	// ── 6. CU-UP ConfigMap + Deployment ──────────────────────────────────────
	// CU-UP binds: N3 (GTP-U→UPF), E1 (E1AP client→CU-CP), F1U (GTP-U→DU).
	// E1 connects to the CU-CP Service, NOT a loopback address.
	cucpSvcDNS := fmt.Sprintf("%s-cucp.%s.svc.cluster.local", cr.Name, cr.Namespace)
	cuupCfgData, err := renderCUUPConfig(CUUPConfigValues{
		E1CUCPAddr:  cucpSvcDNS,
		E1BindAddr:  e1IP,
		N3BindAddr:  n3IP,
		F1UBindAddr: f1uIP,
	})
	if err != nil {
		log.Error(err, "Failed to render CU-UP config")
		return reconcile.Result{}, err
	}
	cuupCMName := cr.Name + "-cuup"
	if err := r.reconcileConfigMap(ctx, cr,
		buildConfigMap(cuupCMName, cr.Namespace, map[string]string{"cu_up.yml": cuupCfgData}),
		log); err != nil {
		return reconcile.Result{}, err
	}
	cuupCM := new(apiv1.ConfigMap)
	_ = r.Client.Get(ctx, types.NamespacedName{Name: cuupCMName, Namespace: cr.Namespace}, cuupCM)
	if err := r.reconcileDeployment(ctx, cr,
		buildCUUPDeployment(cr, cuupCMName, cuupCM.ResourceVersion), log); err != nil {
		return reconcile.Result{}, err
	}

	// ── 7. DU ConfigMap + Deployment ─────────────────────────────────────────
	// DU binds: F1C (F1-AP client→CU-CP), F1U (GTP-U client→CU-UP), ZMQ RF.
	// ZMQ target depends on topology:
	//   single → UE Service DNS
	//   multi  → RadioBreaker Service DNS
	var zmqTarget string
	if topology == srsranov1alpha1.TopologySingle {
		zmqTarget = fmt.Sprintf("%s-ue-0.%s.svc.cluster.local", cr.Name, cr.Namespace)
	} else {
		zmqTarget = fmt.Sprintf("%s-radiobreaker.%s.svc.cluster.local", cr.Name, cr.Namespace)
	}
	duCfgData, err := renderDUConfig(DUConfigValues{
		F1CUCPAddr:  cucpSvcDNS,
		F1CBindAddr: f1cIP,
		F1UBindAddr: f1uIP,
		ZMQTarget:   zmqTarget,
		ZMQTXPort:   zmqBasePort,
		ZMQRXPort:   zmqBasePort + 1,
		PLMN:        cr.Spec.Topology.PLMN,
		TAC:         cr.Spec.Topology.TAC,
		SliceType:   cr.Spec.SliceIntent.Type,
	})
	if err != nil {
		log.Error(err, "Failed to render DU config")
		return reconcile.Result{}, err
	}
	duCMName := cr.Name + "-du"
	if err := r.reconcileConfigMap(ctx, cr,
		buildConfigMap(duCMName, cr.Namespace, map[string]string{"du.yml": duCfgData}),
		log); err != nil {
		return reconcile.Result{}, err
	}
	duCM := new(apiv1.ConfigMap)
	_ = r.Client.Get(ctx, types.NamespacedName{Name: duCMName, Namespace: cr.Namespace}, duCM)
	if err := r.reconcileDeployment(ctx, cr,
		buildDUDeployment(cr, duCMName, qosCMName, duCM.ResourceVersion), log); err != nil {
		return reconcile.Result{}, err
	}

	// ── 8. ZMQ UE topology ───────────────────────────────────────────────────
	switch topology {
	case srsranov1alpha1.TopologySingle:
		// One UE; ZMQ TX/RX port pair 2000/2001, target = DU Service DNS.
		// Note: in single topology the DU uses tcp://<ue-svc>:2000 to reach UE.
		duSvcDNS := fmt.Sprintf("%s-du.%s.svc.cluster.local", cr.Name, cr.Namespace)
		ueCM, err := r.buildUEConfigMap(cr, 0, duSvcDNS, zmqBasePort)
		if err != nil {
			return reconcile.Result{}, err
		}
		if err := r.reconcileConfigMap(ctx, cr, ueCM, log); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.reconcileDeployment(ctx, cr, buildUEDeployment(cr, 0, ueCM.Name), log); err != nil {
			return reconcile.Result{}, err
		}
		// DU Service: exposes ZMQ RX so UE can connect.
		if err := r.reconcileService(ctx, cr, buildDUService(cr), log); err != nil {
			return reconcile.Result{}, err
		}

	case srsranov1alpha1.TopologyMulti:
		// RadioBreaker ZMQ proxy fans out DU → N UEs.
		//   DU  TX → RB RX : tcp://<rb-svc>:2000
		//   DU  RX ← RB TX : tcp://<rb-svc>:2001
		//   UE[i] ZMQ TX/RX: tcp://<rb-svc>:(2000+i*2) / (2001+i*2)
		if err := r.reconcileDeployment(ctx, cr, buildRadioBreakerDeployment(cr), log); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.reconcileService(ctx, cr, buildRadioBreakerService(cr), log); err != nil {
			return reconcile.Result{}, err
		}
		rbSvcDNS := fmt.Sprintf("%s-radiobreaker.%s.svc.cluster.local", cr.Name, cr.Namespace)
		for i := 0; i < cr.Spec.Topology.UECount; i++ {
			uePort := zmqBasePort + i*2
			ueCM, err := r.buildUEConfigMap(cr, i, rbSvcDNS, uePort)
			if err != nil {
				return reconcile.Result{}, err
			}
			if err := r.reconcileConfigMap(ctx, cr, ueCM, log); err != nil {
				return reconcile.Result{}, err
			}
			if err := r.reconcileDeployment(ctx, cr, buildUEDeployment(cr, i, ueCM.Name), log); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// ── 9. Update status ─────────────────────────────────────────────────────
	if err := r.syncStatus(ctx, cr, topology); err != nil {
		log.Error(err, "Failed to sync status")
		return reconcile.Result{}, err
	}

	log.Info("Reconciliation complete")
	return reconcile.Result{}, nil
}

// ─── Topology helper ─────────────────────────────────────────────────────────

func effectiveTopology(cr *srsranov1alpha1.SrsRanDeployment) srsranov1alpha1.TopologyType {
	if cr.Spec.Topology.Type != "" {
		return cr.Spec.Topology.Type
	}
	if cr.Spec.Topology.UECount > 1 {
		return srsranov1alpha1.TopologyMulti
	}
	return srsranov1alpha1.TopologySingle
}

// ─── Resource builders ───────────────────────────────────────────────────────

func buildConfigMap(name, namespace string, data map[string]string) *apiv1.ConfigMap {
	return &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
}

// buildCUCPDeployment returns the CU-CP pod Deployment.
// CU-CP binds N2 (NGAP→AMF), E1 (E1AP server for CU-UP), F1C (F1-AP for DU).
func buildCUCPDeployment(cr *srsranov1alpha1.SrsRanDeployment, cmName, cmVersion string) *appsv1.Deployment {
	replicas := int32(1)
	name := cr.Name + "-cucp"
	img := cr.Spec.CUCPImage
	if img == "" {
		img = "qawl987/srsran-split:latest"
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "cucp")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsFor(cr, "cucp")},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(cr, "cucp"),
					Annotations: map[string]string{ConfigMapVersionAnnotation: cmVersion},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{{
						Name:            "cucp",
						Image:           img,
						ImagePullPolicy: apiv1.PullIfNotPresent,
						Command:         []string{"/usr/local/bin/srscu_cp", "-c", "/srsran/config/cu_cp.yml"},
						SecurityContext: &apiv1.SecurityContext{
							Capabilities: &apiv1.Capabilities{Add: []apiv1.Capability{"NET_ADMIN"}},
						},
						VolumeMounts: []apiv1.VolumeMount{
							{Name: "cucp-config", MountPath: "/srsran/config"},
						},
					}},
					Volumes: []apiv1.Volume{{
						Name: "cucp-config",
						VolumeSource: apiv1.VolumeSource{
							ConfigMap: &apiv1.ConfigMapVolumeSource{
								LocalObjectReference: apiv1.LocalObjectReference{Name: cmName},
							},
						},
					}},
					RestartPolicy: apiv1.RestartPolicyAlways,
				},
			},
		},
	}
}

// buildCUCPService exposes E1AP (port 38462) and F1-AP (port 38472) so that
// CU-UP and DU pods can reach the CU-CP by stable DNS name.
func buildCUCPService(cr *srsranov1alpha1.SrsRanDeployment) *apiv1.Service {
	name := cr.Name + "-cucp"
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "cucp")},
		Spec: apiv1.ServiceSpec{
			Selector: labelsFor(cr, "cucp"),
			Ports: []apiv1.ServicePort{
				{Name: "e1ap", Port: 38462, Protocol: apiv1.ProtocolTCP},
				{Name: "f1ap", Port: 38472, Protocol: apiv1.ProtocolTCP},
			},
			Type: apiv1.ServiceTypeClusterIP,
		},
	}
}

// buildCUUPDeployment returns the CU-UP pod Deployment.
// CU-UP binds N3 (GTP-U→UPF), E1 (E1AP client→CU-CP Service), F1U (GTP-U→DU).
func buildCUUPDeployment(cr *srsranov1alpha1.SrsRanDeployment, cmName, cmVersion string) *appsv1.Deployment {
	replicas := int32(1)
	name := cr.Name + "-cuup"
	img := cr.Spec.CUUPImage
	if img == "" {
		img = "qawl987/srsran-split:latest"
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "cuup")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsFor(cr, "cuup")},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(cr, "cuup"),
					Annotations: map[string]string{ConfigMapVersionAnnotation: cmVersion},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{{
						Name:            "cuup",
						Image:           img,
						ImagePullPolicy: apiv1.PullIfNotPresent,
						Command:         []string{"/usr/local/bin/srscu_up", "-c", "/srsran/config/cu_up.yml"},
						SecurityContext: &apiv1.SecurityContext{
							Capabilities: &apiv1.Capabilities{Add: []apiv1.Capability{"NET_ADMIN"}},
						},
						VolumeMounts: []apiv1.VolumeMount{
							{Name: "cuup-config", MountPath: "/srsran/config"},
						},
					}},
					Volumes: []apiv1.Volume{{
						Name: "cuup-config",
						VolumeSource: apiv1.VolumeSource{
							ConfigMap: &apiv1.ConfigMapVolumeSource{
								LocalObjectReference: apiv1.LocalObjectReference{Name: cmName},
							},
						},
					}},
					RestartPolicy: apiv1.RestartPolicyAlways,
				},
			},
		},
	}
}

// buildDUDeployment returns the DU pod Deployment.
// DU binds F1C (F1-AP→CU-CP), F1U (GTP-U→CU-UP), ZMQ RF (→UE or RadioBreaker).
func buildDUDeployment(cr *srsranov1alpha1.SrsRanDeployment, duCMName, qosCMName, cmVersion string) *appsv1.Deployment {
	replicas := int32(1)
	name := cr.Name + "-du"
	img := cr.Spec.DUImage
	if img == "" {
		img = "qawl987/srsran-split:latest"
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "du")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsFor(cr, "du")},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labelsFor(cr, "du"),
					Annotations: map[string]string{ConfigMapVersionAnnotation: cmVersion},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{{
						Name:            "du",
						Image:           img,
						ImagePullPolicy: apiv1.PullIfNotPresent,
						Command:         []string{"/usr/local/bin/gnb", "-c", "/srsran/config/du.yml"},
						SecurityContext: &apiv1.SecurityContext{
							Capabilities: &apiv1.Capabilities{Add: []apiv1.Capability{"NET_ADMIN"}},
						},
						VolumeMounts: []apiv1.VolumeMount{
							{Name: "du-config", MountPath: "/srsran/config"},
							{Name: "qos-config", MountPath: "/srsran/qos"},
						},
					}},
					Volumes: []apiv1.Volume{
						{
							Name: "du-config",
							VolumeSource: apiv1.VolumeSource{
								ConfigMap: &apiv1.ConfigMapVolumeSource{
									LocalObjectReference: apiv1.LocalObjectReference{Name: duCMName},
								},
							},
						},
						{
							Name: "qos-config",
							VolumeSource: apiv1.VolumeSource{
								ConfigMap: &apiv1.ConfigMapVolumeSource{
									LocalObjectReference: apiv1.LocalObjectReference{Name: qosCMName},
								},
							},
						},
					},
					RestartPolicy: apiv1.RestartPolicyAlways,
				},
			},
		},
	}
}

// buildDUService exposes the DU ZMQ RX port so the single UE pod can connect.
func buildDUService(cr *srsranov1alpha1.SrsRanDeployment) *apiv1.Service {
	name := cr.Name + "-du"
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "du")},
		Spec: apiv1.ServiceSpec{
			Selector: labelsFor(cr, "du"),
			Ports: []apiv1.ServicePort{
				{Name: "zmq-tx", Port: int32(zmqBasePort), Protocol: apiv1.ProtocolTCP},
				{Name: "zmq-rx", Port: int32(zmqBasePort + 1), Protocol: apiv1.ProtocolTCP},
			},
			Type: apiv1.ServiceTypeClusterIP,
		},
	}
}

// buildUEDeployment returns a single UE Deployment for the given index.
func buildUEDeployment(cr *srsranov1alpha1.SrsRanDeployment, index int, ueCMName string) *appsv1.Deployment {
	replicas := int32(1)
	name := fmt.Sprintf("%s-ue-%d", cr.Name, index)
	img := cr.Spec.UEImage
	if img == "" {
		img = "qawl987/srsran-ue:latest"
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsForUE(cr, index)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsForUE(cr, index)},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsForUE(cr, index)},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{{
						Name:            "ue",
						Image:           img,
						ImagePullPolicy: apiv1.PullIfNotPresent,
						Command:         []string{"/usr/local/bin/srsue", "/srsran/config/ue.conf"},
						VolumeMounts: []apiv1.VolumeMount{
							{Name: "ue-config", MountPath: "/srsran/config"},
						},
					}},
					Volumes: []apiv1.Volume{{
						Name: "ue-config",
						VolumeSource: apiv1.VolumeSource{
							ConfigMap: &apiv1.ConfigMapVolumeSource{
								LocalObjectReference: apiv1.LocalObjectReference{Name: ueCMName},
							},
						},
					}},
					RestartPolicy: apiv1.RestartPolicyAlways,
				},
			},
		},
	}
}

// buildRadioBreakerDeployment returns the ZMQ concentrator Deployment.
// In multi-UE topology the DU connects to this proxy; the proxy fans out to N UEs.
// ZMQ port layout:
//
//	DU-facing  TX:2000  RX:2001
//	UE[i]-facing TX:(2000+i*2)  RX:(2001+i*2)
func buildRadioBreakerDeployment(cr *srsranov1alpha1.SrsRanDeployment) *appsv1.Deployment {
	replicas := int32(1)
	name := cr.Name + "-radiobreaker"
	img := cr.Spec.RadioBreakerImage
	if img == "" {
		img = "qawl987/srsran-ue:latest" // RadioBreaker uses the same UE image
	}
	uePorts := ""
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		if i > 0 {
			uePorts += ","
		}
		uePorts += fmt.Sprintf("%d:%d", zmqBasePort+i*2, zmqBasePort+i*2+1)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "radiobreaker")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsFor(cr, "radiobreaker")},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(cr, "radiobreaker")},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{{
						Name:            "radiobreaker",
						Image:           img,
						ImagePullPolicy: apiv1.PullAlways,
						Env: []apiv1.EnvVar{
							{Name: "DU_TX_PORT", Value: fmt.Sprintf("%d", zmqBasePort)},
							{Name: "DU_RX_PORT", Value: fmt.Sprintf("%d", zmqBasePort+1)},
							{Name: "UE_PORTS", Value: uePorts},
							{Name: "NUM_UES", Value: fmt.Sprintf("%d", cr.Spec.Topology.UECount)},
						},
						Ports: buildRadioBreakerContainerPorts(cr.Spec.Topology.UECount),
					}},
					RestartPolicy: apiv1.RestartPolicyAlways,
				},
			},
		},
	}
}

func buildRadioBreakerContainerPorts(ueCount int) []apiv1.ContainerPort {
	ports := []apiv1.ContainerPort{
		{Name: "du-tx", ContainerPort: int32(zmqBasePort), Protocol: apiv1.ProtocolTCP},
		{Name: "du-rx", ContainerPort: int32(zmqBasePort + 1), Protocol: apiv1.ProtocolTCP},
	}
	for i := 0; i < ueCount; i++ {
		ports = append(ports,
			apiv1.ContainerPort{Name: fmt.Sprintf("ue%d-tx", i), ContainerPort: int32(zmqBasePort + i*2), Protocol: apiv1.ProtocolTCP},
			apiv1.ContainerPort{Name: fmt.Sprintf("ue%d-rx", i), ContainerPort: int32(zmqBasePort + i*2 + 1), Protocol: apiv1.ProtocolTCP},
		)
	}
	return ports
}

// buildRadioBreakerService returns a ClusterIP Service exposing all ZMQ ports.
func buildRadioBreakerService(cr *srsranov1alpha1.SrsRanDeployment) *apiv1.Service {
	name := cr.Name + "-radiobreaker"
	ports := []apiv1.ServicePort{
		{Name: "du-tx", Port: int32(zmqBasePort), Protocol: apiv1.ProtocolTCP},
		{Name: "du-rx", Port: int32(zmqBasePort + 1), Protocol: apiv1.ProtocolTCP},
	}
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		ports = append(ports,
			apiv1.ServicePort{Name: fmt.Sprintf("ue%d-tx", i), Port: int32(zmqBasePort + i*2), Protocol: apiv1.ProtocolTCP},
			apiv1.ServicePort{Name: fmt.Sprintf("ue%d-rx", i), Port: int32(zmqBasePort + i*2 + 1), Protocol: apiv1.ProtocolTCP},
		)
	}
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labelsFor(cr, "radiobreaker")},
		Spec: apiv1.ServiceSpec{
			Selector: labelsFor(cr, "radiobreaker"),
			Ports:    ports,
			Type:     apiv1.ServiceTypeClusterIP,
		},
	}
}

// buildUEConfigMap creates the per-UE srsUE config.
// zmqTarget is either the DU Service DNS (single) or RadioBreaker Service DNS (multi).
// zmqTXPort is the port this UE binds its TX to on the target side.
func (r *SrsRanDeploymentReconciler) buildUEConfigMap(
	cr *srsranov1alpha1.SrsRanDeployment, index int, zmqTarget string, zmqTXPort int,
) (*apiv1.ConfigMap, error) {
	ueCfg, err := renderUEConfig(UEConfigValues{
		ZMQTarget: zmqTarget,
		ZMQTXPort: zmqTXPort,
		ZMQRXPort: zmqTXPort + 1,
		PLMN:      cr.Spec.Topology.PLMN,
		FiveQI:    cr.Spec.SliceIntent.FiveQI,
	})
	if err != nil {
		return nil, fmt.Errorf("renderUEConfig[%d]: %w", index, err)
	}
	return buildConfigMap(fmt.Sprintf("%s-ue-%d", cr.Name, index), cr.Namespace,
		map[string]string{"ue.conf": ueCfg}), nil
}

// ─── Reconcile helpers (create-or-update) ───────────────────────────────────

type minimalLogger interface{ Info(string, ...any) }

func (r *SrsRanDeploymentReconciler) reconcileConfigMap(
	ctx context.Context, owner *srsranov1alpha1.SrsRanDeployment,
	desired *apiv1.ConfigMap, log minimalLogger,
) error {
	existing := new(apiv1.ConfigMap)
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if k8serrors.IsNotFound(err) {
		if err2 := ctrl.SetControllerReference(owner, desired, r.Scheme); err2 != nil {
			return err2
		}
		log.Info("Creating ConfigMap", "name", desired.Name)
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Data = desired.Data
	log.Info("Updating ConfigMap", "name", desired.Name)
	return r.Client.Update(ctx, existing)
}

func (r *SrsRanDeploymentReconciler) reconcileDeployment(
	ctx context.Context, owner *srsranov1alpha1.SrsRanDeployment,
	desired *appsv1.Deployment, log minimalLogger,
) error {
	existing := new(appsv1.Deployment)
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if k8serrors.IsNotFound(err) {
		if err2 := ctrl.SetControllerReference(owner, desired, r.Scheme); err2 != nil {
			return err2
		}
		log.Info("Creating Deployment", "name", desired.Name)
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	log.Info("Updating Deployment", "name", desired.Name)
	return r.Client.Update(ctx, desired)
}

func (r *SrsRanDeploymentReconciler) reconcileService(
	ctx context.Context, owner *srsranov1alpha1.SrsRanDeployment,
	desired *apiv1.Service, log minimalLogger,
) error {
	existing := new(apiv1.Service)
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if k8serrors.IsNotFound(err) {
		if err2 := ctrl.SetControllerReference(owner, desired, r.Scheme); err2 != nil {
			return err2
		}
		log.Info("Creating Service", "name", desired.Name)
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.ResourceVersion = existing.ResourceVersion
	log.Info("Updating Service", "name", desired.Name)
	return r.Client.Update(ctx, desired)
}

// ─── Status sync ─────────────────────────────────────────────────────────────

func (r *SrsRanDeploymentReconciler) syncStatus(
	ctx context.Context, cr *srsranov1alpha1.SrsRanDeployment,
	topology srsranov1alpha1.TopologyType,
) error {
	patch := cr.DeepCopy()
	patch.Status.ObservedGeneration = cr.Generation
	patch.Status.ActiveTopology = topology

	var readyUEs int32
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		dep := new(appsv1.Deployment)
		if err := r.Client.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s-ue-%d", cr.Name, i),
			Namespace: cr.Namespace,
		}, dep); err == nil {
			readyUEs += dep.Status.ReadyReplicas
		}
	}
	patch.Status.ReadyUEs = readyUEs

	allReady := readyUEs == int32(cr.Spec.Topology.UECount)
	cond := metav1.Condition{
		Type:               string(srsranov1alpha1.ConditionTypeReady),
		ObservedGeneration: cr.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if allReady {
		cond.Status, cond.Reason = metav1.ConditionTrue, "AllUEsReady"
		cond.Message = fmt.Sprintf("%d/%d UEs ready", readyUEs, cr.Spec.Topology.UECount)
	} else {
		cond.Status, cond.Reason = metav1.ConditionFalse, "UEsNotReady"
		cond.Message = fmt.Sprintf("%d/%d UEs ready", readyUEs, cr.Spec.Topology.UECount)
	}
	patch.Status.Conditions = []metav1.Condition{cond}
	return r.Status().Update(ctx, patch)
}

// ─── Utilities ───────────────────────────────────────────────────────────────

// extractHost parses a CIDR or bare IP and returns just the host part.
// Returns an error if the string is empty or unparseable, which signals that
// Nephio IPAM has not injected this field yet.
func extractHost(cidrOrIP string) (string, error) {
	if cidrOrIP == "" {
		return "", fmt.Errorf("empty IP address")
	}
	if ip, _, err := net.ParseCIDR(cidrOrIP); err == nil {
		return ip.String(), nil
	}
	if ip := net.ParseIP(cidrOrIP); ip != nil {
		return ip.String(), nil
	}
	return "", fmt.Errorf("cannot parse IP/CIDR %q", cidrOrIP)
}

func labelsFor(cr *srsranov1alpha1.SrsRanDeployment, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "srsran",
		"app.kubernetes.io/instance":  cr.Name,
		"app.kubernetes.io/component": component,
	}
}

func labelsForUE(cr *srsranov1alpha1.SrsRanDeployment, index int) map[string]string {
	l := labelsFor(cr, "ue")
	l["srsran.nephio.org/ue-index"] = fmt.Sprintf("%d", index)
	return l
}
