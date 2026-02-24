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
//  2. Determine effective topology (single vs multi) from spec.
//  3. Render and reconcile ConfigMaps (DU config + QoS).
//  4. Reconcile the DU Deployment.
//  5. If single topology → reconcile one UE Deployment.
//     If multi topology  → reconcile RadioBreaker Deployment + N UE Deployments.
//  6. Update status.
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

	// ── 2. Validate IPAM injection ───────────────────────────────────────────
	// The Nephio pipeline (interface-fn + ipam-fn) must have injected real IPs
	// before the operator can proceed. Requeue with backoff if not yet ready.
	n3IP, err := extractHost(cr.Spec.Interfaces.N3.IPAddress)
	if err != nil {
		log.Info("N3 IP not yet injected by Nephio IPAM; requeueing",
			"rawField", cr.Spec.Interfaces.N3.IPAddress)
		return reconcile.Result{RequeueAfter: time.Duration(requeueAfterSeconds) * time.Second}, nil
	}

	f1IP, err := extractHost(cr.Spec.Interfaces.F1.IPAddress)
	if err != nil {
		log.Info("F1 IP not yet injected by Nephio IPAM; requeueing",
			"rawField", cr.Spec.Interfaces.F1.IPAddress)
		return reconcile.Result{RequeueAfter: time.Duration(requeueAfterSeconds) * time.Second}, nil
	}

	// ── 3. Determine effective topology ──────────────────────────────────────
	topology := effectiveTopology(cr)
	log.Info("Resolved topology", "type", topology, "ueCount", cr.Spec.Topology.UECount)

	// ── 4. Reconcile QoS ConfigMap ───────────────────────────────────────────
	qosData, err := renderQoSConfigMap(cr.Spec.SliceIntent)
	if err != nil {
		log.Error(err, "Failed to render QoS ConfigMap")
		return reconcile.Result{}, err
	}
	qosConfigMapName := cr.Name + "-qos"
	qosCM := buildConfigMap(qosConfigMapName, cr.Namespace, map[string]string{
		"qos.yaml": qosData,
	})
	if err := r.reconcileConfigMap(ctx, cr, qosCM, log); err != nil {
		return reconcile.Result{}, err
	}

	// ── 5. Reconcile DU ConfigMap ─────────────────────────────────────────────
	// The DU config ZMQ target depends on topology:
	//   single → point directly at UE (staticlocal IP via loopback/pod IP)
	//   multi  → point at RadioBreaker proxy
	var duZMQTarget string
	if topology == srsranov1alpha1.TopologySingle {
		// In single-UE mode the ZMQ loopback is between two containers in the
		// same namespace; we use 127.0.0.1 as the binding address.
		duZMQTarget = "127.0.0.1"
	} else {
		// Multi-UE: RadioBreaker runs as a separate pod; its Service name is
		// predictable so we use the K8s DNS name.
		duZMQTarget = fmt.Sprintf("%s-radiobreaker.%s.svc.cluster.local",
			cr.Name, cr.Namespace)
	}

	duCfgData, err := renderDUConfig(DUConfigValues{
		F1CUCPAddr: f1IP,
		F1BindAddr: f1IP,
		N3Addr:     n3IP,
		ZMQTarget:  duZMQTarget,
		ZMQTXPort:  zmqBasePort,
		ZMQRXPort:  zmqBasePort + 1,
		PLMN:       cr.Spec.Topology.PLMN,
		TAC:        cr.Spec.Topology.TAC,
		SliceType:  cr.Spec.SliceIntent.Type,
	})
	if err != nil {
		log.Error(err, "Failed to render DU config")
		return reconcile.Result{}, err
	}
	duConfigMapName := cr.Name + "-du"
	duCM := buildConfigMap(duConfigMapName, cr.Namespace, map[string]string{
		"du.yml": duCfgData,
	})
	if err := r.reconcileConfigMap(ctx, cr, duCM, log); err != nil {
		return reconcile.Result{}, err
	}

	// Retrieve the DU ConfigMap version so that pod template annotations force
	// rolling restarts when the ConfigMap changes.
	currentDUCM := new(apiv1.ConfigMap)
	if err := r.Client.Get(ctx,
		types.NamespacedName{Name: duConfigMapName, Namespace: cr.Namespace},
		currentDUCM); err != nil {
		log.Error(err, "Failed to fetch DU ConfigMap after reconcile")
		return reconcile.Result{}, err
	}
	duCMVersion := currentDUCM.ResourceVersion

	// ── 6. Reconcile DU Deployment ───────────────────────────────────────────
	duDeployment := buildDUDeployment(cr, duConfigMapName, qosConfigMapName, duCMVersion)
	if err := r.reconcileDeployment(ctx, cr, duDeployment, log); err != nil {
		return reconcile.Result{}, err
	}

	// ── 7. Reconcile topology-specific resources ──────────────────────────────
	switch topology {
	case srsranov1alpha1.TopologySingle:
		if err := r.reconcileSingleUE(ctx, cr, duConfigMapName, log); err != nil {
			return reconcile.Result{}, err
		}

	case srsranov1alpha1.TopologyMulti:
		if err := r.reconcileMultiUE(ctx, cr, log); err != nil {
			return reconcile.Result{}, err
		}
	}

	// ── 8. Update status ─────────────────────────────────────────────────────
	if err := r.syncStatus(ctx, cr, topology); err != nil {
		log.Error(err, "Failed to sync status")
		return reconcile.Result{}, err
	}

	log.Info("Reconciliation complete")
	return reconcile.Result{}, nil
}

// ─── Topology helpers ────────────────────────────────────────────────────────

// effectiveTopology returns the actual topology to deploy.
// If the user explicitly sets spec.topology.type it is honoured; otherwise
// it is inferred from ueCount.
func effectiveTopology(cr *srsranov1alpha1.SrsRanDeployment) srsranov1alpha1.TopologyType {
	if cr.Spec.Topology.Type != "" {
		return cr.Spec.Topology.Type
	}
	if cr.Spec.Topology.UECount > 1 {
		return srsranov1alpha1.TopologyMulti
	}
	return srsranov1alpha1.TopologySingle
}

// ─── Single-UE topology ───────────────────────────────────────────────────────

// reconcileSingleUE creates/updates one UE Deployment wired directly to the DU
// via ZMQ on localhost ports 2000/2001.
func (r *SrsRanDeploymentReconciler) reconcileSingleUE(
	ctx context.Context,
	cr *srsranov1alpha1.SrsRanDeployment,
	duConfigMapName string,
	log interface{ Info(string, ...any) },
) error {
	ueCM, err := r.buildUEConfigMap(cr, 0, "127.0.0.1", zmqBasePort)
	if err != nil {
		return err
	}
	if err := r.reconcileConfigMap(ctx, cr, ueCM, log); err != nil {
		return err
	}
	ueDeployment := buildUEDeployment(cr, 0, ueCM.Name)
	return r.reconcileDeployment(ctx, cr, ueDeployment, log)
}

// ─── Multi-UE topology ────────────────────────────────────────────────────────

// reconcileMultiUE creates/updates:
//  1. A RadioBreaker Deployment — a ZMQ proxy that fans-out from the DU to N UEs.
//  2. N UE Deployments, each pointing at the RadioBreaker with incrementing ports.
//
// ZMQ port assignment:
//
//	DU TX  → RadioBreaker RX : tcp://<rb>:2000
//	DU RX  ← RadioBreaker TX : tcp://<rb>:2001
//	UE[i]  → RadioBreaker RX : tcp://<rb>:(2000 + i*2)
//	UE[i]  ← RadioBreaker TX : tcp://<rb>:(2001 + i*2)
func (r *SrsRanDeploymentReconciler) reconcileMultiUE(
	ctx context.Context,
	cr *srsranov1alpha1.SrsRanDeployment,
	log interface{ Info(string, ...any) },
) error {
	rbName := cr.Name + "-radiobreaker"
	rbServiceDNS := fmt.Sprintf("%s.%s.svc.cluster.local", rbName, cr.Namespace)

	// 7a. RadioBreaker Deployment
	rbDeployment := buildRadioBreakerDeployment(cr)
	if err := r.reconcileDeployment(ctx, cr, rbDeployment, log); err != nil {
		return err
	}

	// 7b. RadioBreaker Service (ClusterIP so UEs can reach it by DNS)
	rbService := buildRadioBreakerService(cr)
	if err := r.reconcileService(ctx, cr, rbService, log); err != nil {
		return err
	}

	// 7c. N UE Deployments
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		uePort := zmqBasePort + i*2
		ueCM, err := r.buildUEConfigMap(cr, i, rbServiceDNS, uePort)
		if err != nil {
			return err
		}
		if err := r.reconcileConfigMap(ctx, cr, ueCM, log); err != nil {
			return err
		}
		ueDeployment := buildUEDeployment(cr, i, ueCM.Name)
		if err := r.reconcileDeployment(ctx, cr, ueDeployment, log); err != nil {
			return err
		}
	}
	return nil
}

// ─── Resource builders ───────────────────────────────────────────────────────

// buildConfigMap is a convenience constructor.
func buildConfigMap(name, namespace string, data map[string]string) *apiv1.ConfigMap {
	return &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
}

// buildDUDeployment returns the DU gNB Deployment object.
func buildDUDeployment(
	cr *srsranov1alpha1.SrsRanDeployment,
	duCMName, qosCMName, cmVersion string,
) *appsv1.Deployment {
	replicas := int32(1)
	name := cr.Name + "-du"

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    labelsFor(cr, "du"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsFor(cr, "du")},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelsFor(cr, "du"),
					Annotations: map[string]string{
						ConfigMapVersionAnnotation: cmVersion,
					},
				},
				Spec: apiv1.PodSpec{
					// NET_ADMIN is required for GTP-U tunnel creation.
					InitContainers: []apiv1.Container{},
					Containers: []apiv1.Container{
						{
							Name:            "du",
							Image:           cr.Spec.DUImage,
							ImagePullPolicy: apiv1.PullAlways,
							SecurityContext: &apiv1.SecurityContext{
								Capabilities: &apiv1.Capabilities{
									Add: []apiv1.Capability{"NET_ADMIN"},
								},
							},
							Command: []string{
								"/usr/local/bin/gnb",
								"-c", "/srsran/config/du.yml",
							},
							VolumeMounts: []apiv1.VolumeMount{
								{Name: "du-config", MountPath: "/srsran/config"},
								{Name: "qos-config", MountPath: "/srsran/qos"},
							},
						},
					},
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

// buildUEDeployment returns a single UE Deployment. Index i is used to give
// each pod a unique name and to distinguish log output.
func buildUEDeployment(cr *srsranov1alpha1.SrsRanDeployment, index int, ueCMName string) *appsv1.Deployment {
	replicas := int32(1)
	name := fmt.Sprintf("%s-ue-%d", cr.Name, index)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    labelsForUE(cr, index),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsForUE(cr, index)},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsForUE(cr, index)},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:            "ue",
							Image:           cr.Spec.UEImage,
							ImagePullPolicy: apiv1.PullAlways,
							Command:         []string{"/usr/local/bin/srsue", "/srsran/config/ue.conf"},
							VolumeMounts: []apiv1.VolumeMount{
								{Name: "ue-config", MountPath: "/srsran/config"},
							},
						},
					},
					Volumes: []apiv1.Volume{
						{
							Name: "ue-config",
							VolumeSource: apiv1.VolumeSource{
								ConfigMap: &apiv1.ConfigMapVolumeSource{
									LocalObjectReference: apiv1.LocalObjectReference{Name: ueCMName},
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

// buildRadioBreakerDeployment returns a ZMQ concentrator (RadioBreaker) Deployment.
// The RadioBreaker acts as a transparent ZMQ proxy between the DU and N UEs,
// allowing the DU to communicate with one ZMQ socket regardless of UE count.
func buildRadioBreakerDeployment(cr *srsranov1alpha1.SrsRanDeployment) *appsv1.Deployment {
	replicas := int32(1)
	name := cr.Name + "-radiobreaker"

	// Build the ZMQ port list for the N UEs so they can be passed as env vars
	// to the RadioBreaker. Each UE pair: (2000+i*2, 2001+i*2).
	uePorts := ""
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		if i > 0 {
			uePorts += ","
		}
		uePorts += fmt.Sprintf("%d:%d", zmqBasePort+i*2, zmqBasePort+i*2+1)
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    labelsFor(cr, "radiobreaker"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsFor(cr, "radiobreaker")},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(cr, "radiobreaker")},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:            "radiobreaker",
							Image:           cr.Spec.RadioBreakerImage,
							ImagePullPolicy: apiv1.PullAlways,
							Env: []apiv1.EnvVar{
								// DU-facing ZMQ socket addresses.
								{Name: "DU_TX_PORT", Value: fmt.Sprintf("%d", zmqBasePort)},
								{Name: "DU_RX_PORT", Value: fmt.Sprintf("%d", zmqBasePort+1)},
								// UE-facing port pairs: "2000:2001,2002:2003,..."
								{Name: "UE_PORTS", Value: uePorts},
								{Name: "NUM_UES", Value: fmt.Sprintf("%d", cr.Spec.Topology.UECount)},
							},
							Ports: buildRadioBreakerContainerPorts(cr.Spec.Topology.UECount),
						},
					},
					RestartPolicy: apiv1.RestartPolicyAlways,
				},
			},
		},
	}
}

// buildRadioBreakerContainerPorts exposes all ZMQ ports so the K8s Service
// can route them correctly.
func buildRadioBreakerContainerPorts(ueCount int) []apiv1.ContainerPort {
	// Always expose the DU-facing ports.
	ports := []apiv1.ContainerPort{
		{Name: "du-tx", ContainerPort: int32(zmqBasePort), Protocol: apiv1.ProtocolTCP},
		{Name: "du-rx", ContainerPort: int32(zmqBasePort + 1), Protocol: apiv1.ProtocolTCP},
	}
	// Expose per-UE ports.
	for i := 0; i < ueCount; i++ {
		txPort := int32(zmqBasePort + i*2)
		rxPort := int32(zmqBasePort + i*2 + 1)
		ports = append(ports,
			apiv1.ContainerPort{
				Name:          fmt.Sprintf("ue%d-tx", i),
				ContainerPort: txPort,
				Protocol:      apiv1.ProtocolTCP,
			},
			apiv1.ContainerPort{
				Name:          fmt.Sprintf("ue%d-rx", i),
				ContainerPort: rxPort,
				Protocol:      apiv1.ProtocolTCP,
			},
		)
	}
	return ports
}

// buildRadioBreakerService returns a ClusterIP Service for the RadioBreaker
// so that UEs can reach it via predictable DNS.
func buildRadioBreakerService(cr *srsranov1alpha1.SrsRanDeployment) *apiv1.Service {
	name := cr.Name + "-radiobreaker"
	ports := []apiv1.ServicePort{
		{Name: "du-tx", Port: int32(zmqBasePort), Protocol: apiv1.ProtocolTCP},
		{Name: "du-rx", Port: int32(zmqBasePort + 1), Protocol: apiv1.ProtocolTCP},
	}
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		ports = append(ports,
			apiv1.ServicePort{
				Name:     fmt.Sprintf("ue%d-tx", i),
				Port:     int32(zmqBasePort + i*2),
				Protocol: apiv1.ProtocolTCP,
			},
			apiv1.ServicePort{
				Name:     fmt.Sprintf("ue%d-rx", i),
				Port:     int32(zmqBasePort + i*2 + 1),
				Protocol: apiv1.ProtocolTCP,
			},
		)
	}
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    labelsFor(cr, "radiobreaker"),
		},
		Spec: apiv1.ServiceSpec{
			Selector: labelsFor(cr, "radiobreaker"),
			Ports:    ports,
			Type:     apiv1.ServiceTypeClusterIP,
		},
	}
}

// buildUEConfigMap creates a per-UE ConfigMap with the srsUE conf file filled
// in with the appropriate ZMQ TX/RX addresses.
//
// zmqTarget is either "127.0.0.1" (single topology) or the RadioBreaker
// service DNS name (multi topology).
// zmqTXPort is the port this UE uses to TX to the DU/RadioBreaker.
func (r *SrsRanDeploymentReconciler) buildUEConfigMap(
	cr *srsranov1alpha1.SrsRanDeployment,
	index int,
	zmqTarget string,
	zmqTXPort int,
) (*apiv1.ConfigMap, error) {
	zmqRXPort := zmqTXPort + 1
	ueCfg, err := renderUEConfig(UEConfigValues{
		ZMQTarget: zmqTarget,
		ZMQTXPort: zmqTXPort,
		ZMQRXPort: zmqRXPort,
		PLMN:      cr.Spec.Topology.PLMN,
		FiveQI:    cr.Spec.SliceIntent.FiveQI,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render UE[%d] config: %w", index, err)
	}
	cmName := fmt.Sprintf("%s-ue-%d", cr.Name, index)
	return buildConfigMap(cmName, cr.Namespace, map[string]string{"ue.conf": ueCfg}), nil
}

// ─── Reconcile helpers (create-or-update pattern) ───────────────────────────

type minimalLogger interface{ Info(string, ...any) }

func (r *SrsRanDeploymentReconciler) reconcileConfigMap(
	ctx context.Context,
	owner *srsranov1alpha1.SrsRanDeployment,
	desired *apiv1.ConfigMap,
	log minimalLogger,
) error {
	existing := new(apiv1.ConfigMap)
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if k8serrors.IsNotFound(err) {
		if setErr := ctrl.SetControllerReference(owner, desired, r.Scheme); setErr != nil {
			return setErr
		}
		log.Info("Creating ConfigMap", "name", desired.Name)
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Update data in place.
	existing.Data = desired.Data
	log.Info("Updating ConfigMap", "name", desired.Name)
	return r.Client.Update(ctx, existing)
}

func (r *SrsRanDeploymentReconciler) reconcileDeployment(
	ctx context.Context,
	owner *srsranov1alpha1.SrsRanDeployment,
	desired *appsv1.Deployment,
	log minimalLogger,
) error {
	existing := new(appsv1.Deployment)
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if k8serrors.IsNotFound(err) {
		if setErr := ctrl.SetControllerReference(owner, desired, r.Scheme); setErr != nil {
			return setErr
		}
		log.Info("Creating Deployment", "name", desired.Name)
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Preserve resource version and update spec.
	desired.ResourceVersion = existing.ResourceVersion
	log.Info("Updating Deployment", "name", desired.Name)
	return r.Client.Update(ctx, desired)
}

func (r *SrsRanDeploymentReconciler) reconcileService(
	ctx context.Context,
	owner *srsranov1alpha1.SrsRanDeployment,
	desired *apiv1.Service,
	log minimalLogger,
) error {
	existing := new(apiv1.Service)
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if k8serrors.IsNotFound(err) {
		if setErr := ctrl.SetControllerReference(owner, desired, r.Scheme); setErr != nil {
			return setErr
		}
		log.Info("Creating Service", "name", desired.Name)
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Preserve ClusterIP assigned by Kubernetes.
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.ResourceVersion = existing.ResourceVersion
	log.Info("Updating Service", "name", desired.Name)
	return r.Client.Update(ctx, desired)
}

// ─── Status sync ─────────────────────────────────────────────────────────────

func (r *SrsRanDeploymentReconciler) syncStatus(
	ctx context.Context,
	cr *srsranov1alpha1.SrsRanDeployment,
	topology srsranov1alpha1.TopologyType,
) error {
	patch := cr.DeepCopy()
	patch.Status.ObservedGeneration = cr.Generation
	patch.Status.ActiveTopology = topology

	// Count ready UE pods.
	var readyUEs int32
	for i := 0; i < cr.Spec.Topology.UECount; i++ {
		dep := new(appsv1.Deployment)
		name := fmt.Sprintf("%s-ue-%d", cr.Name, i)
		if err := r.Client.Get(ctx,
			types.NamespacedName{Name: name, Namespace: cr.Namespace}, dep); err == nil {
			readyUEs += dep.Status.ReadyReplicas
		}
	}
	patch.Status.ReadyUEs = readyUEs

	readyCond := metav1.Condition{
		Type:               string(srsranov1alpha1.ConditionTypeReady),
		ObservedGeneration: cr.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if readyUEs == int32(cr.Spec.Topology.UECount) {
		readyCond.Status = metav1.ConditionTrue
		readyCond.Reason = "AllUEsReady"
		readyCond.Message = fmt.Sprintf("%d/%d UEs ready", readyUEs, cr.Spec.Topology.UECount)
	} else {
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = "UEsNotReady"
		readyCond.Message = fmt.Sprintf("%d/%d UEs ready", readyUEs, cr.Spec.Topology.UECount)
	}
	patch.Status.Conditions = []metav1.Condition{readyCond}

	return r.Status().Update(ctx, patch)
}

// ─── Utilities ───────────────────────────────────────────────────────────────

// extractHost parses a CIDR or bare IP and returns the host portion only.
// Returns an error if the input is empty or unparseable (used to detect
// whether Nephio IPAM has injected the IP yet).
func extractHost(cidrOrIP string) (string, error) {
	if cidrOrIP == "" {
		return "", fmt.Errorf("empty IP address")
	}
	// Try CIDR first.
	ip, _, err := net.ParseCIDR(cidrOrIP)
	if err == nil {
		return ip.String(), nil
	}
	// Bare IP.
	if parsed := net.ParseIP(cidrOrIP); parsed != nil {
		return parsed.String(), nil
	}
	return "", fmt.Errorf("cannot parse IP/CIDR %q", cidrOrIP)
}

// labelsFor returns the common label set for a component within an SrsRanDeployment.
func labelsFor(cr *srsranov1alpha1.SrsRanDeployment, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "srsran",
		"app.kubernetes.io/instance":  cr.Name,
		"app.kubernetes.io/component": component,
	}
}

// labelsForUE returns per-UE labels that include the UE index.
func labelsForUE(cr *srsranov1alpha1.SrsRanDeployment, index int) map[string]string {
	l := labelsFor(cr, "ue")
	l["srsran.nephio.org/ue-index"] = fmt.Sprintf("%d", index)
	return l
}
