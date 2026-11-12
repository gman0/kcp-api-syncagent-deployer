/*
Copyright 2025 The Platform Mesh Authors.

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

package wildwest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlmanger "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	deployHelmRepoGVK = schema.GroupVersionKind{
		Group:   "source.toolkit.fluxcd.io",
		Version: "v1",
		Kind:    "HelmRepository",
	}
	deployHelmReleaseGVK = schema.GroupVersionKind{
		Group:   "helm.toolkit.fluxcd.io",
		Version: "v2",
		Kind:    "HelmRelease",
	}
)

const (
	// AnnotationAgentName optionally overrides the agent name; defaults to the secret name.
	AnnotationAgentName = "apisyncagent.platform-mesh.io/agent-name"

	helmRepoName    = "kcp"
	helmRepoURL     = "https://kcp-dev.github.io/helm-charts"
	helmChartName   = "api-syncagent"
	helmChartVer    = "0.6.2"
	releasePrefix   = "api-syncagent-"
	managedByLabel  = "app.kubernetes.io/managed-by"
	managedByValue  = "apisyncagent-deployer"
	ownerSecretAnno = "apisyncagent.platform-mesh.io/owner-secret"
)

type DeployerReconciler struct {
	Manager ctrlmanger.Manager
	// APIExportEndpointSlice is the name passed to every HelmRelease as
	// chart value `apiExportEndpointSliceName`.
	APIExportEndpointSlice string
	// SecretSelector, if non-nil, restricts reconciliation to Secrets whose
	// labels match it. nil means all Secrets are reconciled.
	SecretSelector labels.Selector
}

func (r *DeployerReconciler) SetupWithManager(mgr ctrlmanger.Manager) error {
	if r.APIExportEndpointSlice == "" {
		return fmt.Errorf("APIExportEndpointSlice must be set")
	}
	r.Manager = mgr

	b := ctrlbuilder.ControllerManagedBy(mgr).
		Named("deployer-controller").
		For(&corev1.Secret{})

	if r.SecretSelector != nil {
		b = b.WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return r.SecretSelector.Matches(labels.Set(obj.GetLabels()))
		}))
	}

	return b.Complete(ctrlreconcile.Func(r.Reconcile))
}

func (r *DeployerReconciler) Reconcile(ctx context.Context, req ctrlreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	c := r.Manager.GetClient()

	kubeconfigSecret := &corev1.Secret{}
	if err := c.Get(ctx, req.NamespacedName, kubeconfigSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, ensureApisyncagentHelmReleaseDeleted(ctx, c, req.NamespacedName)
		}
		return reconcile.Result{}, fmt.Errorf("failed to get secret: %w", err)
	}

	if !kubeconfigSecret.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, ensureApisyncagentHelmReleaseDeleted(ctx, c, req.NamespacedName)
	}

	logger.Info("Reconciling kubeconfig Secret", "endpointSlice", r.APIExportEndpointSlice)

	if err := ensureApisyncagentHelmRepo(ctx, c, kubeconfigSecret.Namespace); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to ensure HelmRepository: %w", err)
	}

	agentName := kubeconfigSecret.Annotations[AnnotationAgentName]
	if agentName == "" {
		agentName = kubeconfigSecret.Name
	}

	if err := ensureApisyncagentHelmRelease(ctx, c, kubeconfigSecret, r.APIExportEndpointSlice, agentName); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to ensure HelmRelease: %w", err)
	}

	return reconcile.Result{}, nil
}

func ensureApisyncagentHelmRepo(ctx context.Context, c client.Client, namespace string) error {
	repo := &unstructured.Unstructured{}
	repo.SetGroupVersionKind(deployHelmRepoGVK)
	repo.SetNamespace(namespace)
	repo.SetName(helmRepoName)

	_, err := controllerutil.CreateOrUpdate(ctx, c, repo, func() error {
		repo.SetLabels(map[string]string{managedByLabel: managedByValue})
		if err := unstructured.SetNestedField(repo.Object, helmRepoURL, "spec", "url"); err != nil {
			return err
		}
		return unstructured.SetNestedField(repo.Object, "10m", "spec", "interval")
	})
	return err
}

func ensureApisyncagentHelmRelease(ctx context.Context, c client.Client, secret *corev1.Secret, endpointSlice, agentName string) error {
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(deployHelmReleaseGVK)
	release.SetNamespace(secret.Namespace)
	release.SetName(releasePrefix + secret.Name)

	_, err := controllerutil.CreateOrUpdate(ctx, c, release, func() error {
		release.SetLabels(map[string]string{managedByLabel: managedByValue})
		annos := release.GetAnnotations()
		if annos == nil {
			annos = map[string]string{}
		}
		annos[ownerSecretAnno] = secret.Name
		release.SetAnnotations(annos)

		if err := controllerutil.SetControllerReference(secret, release, c.Scheme()); err != nil {
			return err
		}

		spec := map[string]any{
			"interval":        "10m",
			"releaseName":     releasePrefix + secret.Name,
			"targetNamespace": secret.Namespace,
			"chart": map[string]any{
				"spec": map[string]any{
					"chart":   helmChartName,
					"version": helmChartVer,
					"sourceRef": map[string]any{
						"kind":      "HelmRepository",
						"name":      helmRepoName,
						"namespace": secret.Namespace,
					},
				},
			},
			"install": map[string]any{
				"createNamespace": true,
			},
			"values": map[string]any{
				"apiExportEndpointSliceName": endpointSlice,
				"agentName":                  agentName,
				"kcpKubeconfig":              secret.Name,
			},
		}
		return unstructured.SetNestedField(release.Object, spec, "spec")
	})
	return err
}

// HelmRepository is intentionally left in place; it is shared across releases.
func ensureApisyncagentHelmReleaseDeleted(ctx context.Context, c client.Client, secretKey types.NamespacedName) error {
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(deployHelmReleaseGVK)
	release.SetNamespace(secretKey.Namespace)
	release.SetName(releasePrefix + secretKey.Name)

	if err := c.Delete(ctx, release, &client.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
