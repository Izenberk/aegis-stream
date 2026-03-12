/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	streamv1alpha1 "aegis-stream/operator/api/v1alpha1"
)

// AegisPipelineReconciler reconciles a AegisPipeline object.
// It watches for AegisPipeline CRs and ensures a matching Deployment + Service
// exist in the cluster. This is the "control loop" pattern:
//   - User creates/updates an AegisPipeline CR (desired state)
//   - Reconcile compares desired vs actual state
//   - Reconcile creates or patches resources to close the gap
type AegisPipelineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC markers — kubebuilder reads these comments and generates ClusterRole rules.
// The operator needs permission to manage these resource types.

// +kubebuilder:rbac:groups=stream.aegis.io,resources=aegispipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stream.aegis.io,resources=aegispipelines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stream.aegis.io,resources=aegispipelines/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile runs every time an AegisPipeline CR changes (create, update, delete).
// It follows the "level-triggered" pattern: instead of reacting to individual events,
// it compares desired state (the CR spec) with actual state (what's in the cluster)
// and takes action to close any gap.
func (r *AegisPipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Step 1: Fetch the AegisPipeline CR that triggered this reconciliation.
	// If it was deleted, we get a NotFound error — that's fine, K8s garbage
	// collection (via ownerReferences) will clean up the Deployment and Service.
	var pipeline streamv1alpha1.AegisPipeline
	if err := r.Get(ctx, req.NamespacedName, &pipeline); err != nil {
		if apierrors.IsNotFound(err) {
			// CR was deleted — child resources will be garbage-collected
			// because we set ownerReferences on them (see below).
			log.Info("AegisPipeline deleted, owned resources will be garbage-collected")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Step 2: Build the desired Deployment from the CR spec.
	// This is the "desired state" — what the cluster should look like.
	desiredDeploy := r.buildDeployment(&pipeline)

	// Step 3: Create or update the Deployment.
	// CreateOrUpdate is an idempotent operation:
	//   - If the Deployment doesn't exist → create it
	//   - If it exists but differs from desired → update it
	//   - If it already matches → do nothing
	// The MutateFn (callback) runs BEFORE the create/update, letting us
	// set the spec fields we care about while preserving K8s-managed fields.
	var deploy appsv1.Deployment
	deploy.Name = desiredDeploy.Name
	deploy.Namespace = desiredDeploy.Namespace

	deployResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, &deploy, func() error {
		// Set the ownerReference so K8s knows this Deployment belongs to the CR.
		// When the CR is deleted, K8s automatically deletes this Deployment.
		if err := controllerutil.SetControllerReference(&pipeline, &deploy, r.Scheme); err != nil {
			return err
		}
		// Copy the desired spec into the actual Deployment.
		deploy.Labels = desiredDeploy.Labels
		deploy.Spec = desiredDeploy.Spec
		return nil
	})
	if err != nil {
		log.Error(err, "failed to reconcile Deployment")
		return ctrl.Result{}, err
	}
	log.Info("Deployment reconciled", "result", deployResult)

	// Step 4: Build and reconcile the Service the same way.
	desiredSvc := r.buildService(&pipeline)

	var svc corev1.Service
	svc.Name = desiredSvc.Name
	svc.Namespace = desiredSvc.Namespace

	svcResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, &svc, func() error {
		if err := controllerutil.SetControllerReference(&pipeline, &svc, r.Scheme); err != nil {
			return err
		}
		svc.Labels = desiredSvc.Labels
		// Preserve the ClusterIP assigned by K8s — it's immutable after creation.
		// We only update the fields we control.
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		svc.Spec.Type = desiredSvc.Spec.Type
		return nil
	})
	if err != nil {
		log.Error(err, "failed to reconcile Service")
		return ctrl.Result{}, err
	}
	log.Info("Service reconciled", "result", svcResult)

	// Step 5: Update the CR status so users can see what's actually running.
	// We read the Deployment's status to get the current ready replica count.
	if err := r.Get(ctx, client.ObjectKeyFromObject(&deploy), &deploy); err != nil {
		return ctrl.Result{}, err
	}

	pipeline.Status.ReadyReplicas = deploy.Status.ReadyReplicas
	pipeline.Status.Phase = computePhase(deploy.Status, pipeline.Spec.Replicas)

	if err := r.Status().Update(ctx, &pipeline); err != nil {
		log.Error(err, "failed to update AegisPipeline status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// computePhase derives a human-readable phase from the Deployment status.
// This is what shows up in the "Phase" column when you run kubectl get aegispipelines.
func computePhase(status appsv1.DeploymentStatus, desiredReplicas int32) string {
	if status.ReadyReplicas == desiredReplicas {
		return "Running"
	}
	if status.ReadyReplicas > 0 {
		return "Pending" // some pods ready, but not all
	}
	return "Pending"
}

// buildDeployment constructs the desired Deployment object from the CR spec.
// This mirrors our hand-written k8s/deployment.yaml but is now generated
// programmatically from the CRD fields.
func (r *AegisPipelineReconciler) buildDeployment(p *streamv1alpha1.AegisPipeline) *appsv1.Deployment {
	// Labels tie the Deployment, Pod template, and Service together.
	labels := map[string]string{
		"app":                          p.Name,
		"app.kubernetes.io/managed-by": "aegis-operator",
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &p.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": p.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// Prometheus annotations — same ones from our manual deployment.yaml.
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/port":   fmt.Sprintf("%d", p.Spec.MetricsPort),
						"prometheus.io/path":   "/metrics",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "aegis-stream",
						Image:           p.Spec.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{Name: "tcp-data", ContainerPort: p.Spec.Port},
							{Name: "http-metrics", ContainerPort: p.Spec.MetricsPort},
						},
						// Map CRD fields to the env vars our server reads (internal/config).
						Env: r.buildEnvVars(p),
						// Same probes we had in the manual YAML.
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("http-metrics"),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("http-metrics"),
								},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       5,
						},
					}},
				},
			},
		},
	}
}

// buildEnvVars constructs the env var list from the CR spec.
// Maps CRD fields to the env vars our server reads (internal/config).
func (r *AegisPipelineReconciler) buildEnvVars(p *streamv1alpha1.AegisPipeline) []corev1.EnvVar {
	envs := []corev1.EnvVar{
		{Name: "AEGIS_PORT", Value: fmt.Sprintf(":%d", p.Spec.Port)},
		{Name: "AEGIS_WORKERS", Value: fmt.Sprintf("%d", p.Spec.Workers)},
		{Name: "AEGIS_QUEUE_DEPTH", Value: fmt.Sprintf("%d", p.Spec.QueueDepth)},
		{Name: "AEGIS_METRICS_PORT", Value: fmt.Sprintf(":%d", p.Spec.MetricsPort)},
	}

	if p.Spec.SinkType != "" {
		envs = append(envs, corev1.EnvVar{Name: "AEGIS_SINK", Value: p.Spec.SinkType})
	}
	if p.Spec.PostgresURL != "" {
		envs = append(envs, corev1.EnvVar{Name: "AEGIS_POSTGRES_URL", Value: p.Spec.PostgresURL})
	}
	if p.Spec.NATSURL != "" {
		envs = append(envs, corev1.EnvVar{Name: "AEGIS_NATS_URL", Value: p.Spec.NATSURL})
	}

	return envs
}

// buildService constructs the desired Service from the CR spec.
// Same structure as our manual k8s/service.yaml.
func (r *AegisPipelineReconciler) buildService(p *streamv1alpha1.AegisPipeline) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels: map[string]string{
				"app":                          p.Name,
				"app.kubernetes.io/managed-by": "aegis-operator",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": p.Name},
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "tcp-data",
					Port:       p.Spec.Port,
					TargetPort: intstr.FromString("tcp-data"),
				},
				{
					Name:       "http-metrics",
					Port:       p.Spec.MetricsPort,
					TargetPort: intstr.FromString("http-metrics"),
				},
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
// For() watches AegisPipeline CRs — any create/update/delete triggers Reconcile.
// Owns() watches Deployments and Services owned by an AegisPipeline — if someone
// manually deletes a Deployment, the controller detects it and recreates it.
func (r *AegisPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&streamv1alpha1.AegisPipeline{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("aegispipeline").
		Complete(r)
}
