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
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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
	publishedResourceGVK = schema.GroupVersionKind{
		Group:   "syncagent.kcp.io",
		Version: "v1alpha1",
		Kind:    "PublishedResource",
	}
	publishedResourceListGVK = schema.GroupVersionKind{
		Group:   "syncagent.kcp.io",
		Version: "v1alpha1",
		Kind:    "PublishedResourceList",
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

	// agentNameLabel is the standard label api-syncagent recognizes on
	// PublishedResources for per-agent scoping. Must match the key defined by
	// github.com/kcp-dev/api-syncagent/sdk/apis/syncagent/v1alpha1
	// (AgentNameLabel). Each deployed agent gets a
	// `--published-resource-selector` built off this key so that multiple
	// agents on the same provider cluster don't clash.
	agentNameLabel = "syncagent.kcp.io/agent-name"

	// pubResTemplateLabel marks a PublishedResource as a user-authored
	// template. The deployer fans each template out into per-agent copies;
	// no api-syncagent instance ever reconciles the template itself because
	// its publishedResourceSelector only matches copies.
	pubResTemplateLabel = "apisyncagent.platform-mesh.io/template"

	// pubResTemplateSourceLabel records which template a generated copy
	// came from. Used to prune copies whose template was deleted.
	pubResTemplateSourceLabel = "apisyncagent.platform-mesh.io/template-source"
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
			// Secrets are the primary For() target; the SecretSelector only
			// needs to apply to them. Any other watched kind (templates) is
			// filtered in its own predicate below.
			if _, isSecret := obj.(*corev1.Secret); !isSecret {
				return true
			}
			return r.SecretSelector.Matches(labels.Set(obj.GetLabels()))
		}))
	}

	// Watch template PublishedResources — a change to any template enqueues
	// every (matching) Secret so the fan-out stays in sync.
	tmplObj := &unstructured.Unstructured{}
	tmplObj.SetGroupVersionKind(publishedResourceGVK)
	b = b.Watches(tmplObj,
		handler.EnqueueRequestsFromMapFunc(r.enqueueSecretsForTemplate),
		ctrlbuilder.WithPredicates(predicate.NewPredicateFuncs(isTemplate)))

	return b.Complete(ctrlreconcile.Func(r.Reconcile))
}

func isTemplate(obj client.Object) bool {
	return obj.GetLabels()[pubResTemplateLabel] == "true"
}

func (r *DeployerReconciler) enqueueSecretsForTemplate(ctx context.Context, _ client.Object) []reconcile.Request {
	c := r.Manager.GetClient()
	secrets := &corev1.SecretList{}
	if err := c.List(ctx, secrets); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Secrets for template fan-out")
		return nil
	}
	out := make([]reconcile.Request, 0, len(secrets.Items))
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if r.SecretSelector != nil && !r.SecretSelector.Matches(labels.Set(s.GetLabels())) {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
	}
	return out
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

	if err := r.reconcilePublishedResourceCopies(ctx, kubeconfigSecret, agentName); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile PublishedResource copies: %w", err)
	}

	return reconcile.Result{}, nil
}

// reconcilePublishedResourceCopies fans every template PublishedResource in
// the Secret's namespace out into a per-agent copy, and prunes any
// previously generated copy whose source template no longer exists.
func (r *DeployerReconciler) reconcilePublishedResourceCopies(ctx context.Context, secret *corev1.Secret, agentName string) error {
	c := r.Manager.GetClient()

	// 1. Fan out: for each template, ensure a copy for this agent.
	templates := &unstructured.UnstructuredList{}
	templates.SetGroupVersionKind(publishedResourceListGVK)
	if err := c.List(ctx, templates,
		client.InNamespace(secret.Namespace),
		client.MatchingLabels{pubResTemplateLabel: "true"},
	); err != nil {
		return fmt.Errorf("list template PublishedResources: %w", err)
	}
	templateNames := make(map[string]struct{}, len(templates.Items))
	var errs []error
	for i := range templates.Items {
		tmpl := &templates.Items[i]
		templateNames[tmpl.GetName()] = struct{}{}
		if err := ensurePublishedResourceCopy(ctx, c, secret, agentName, tmpl); err != nil {
			errs = append(errs, fmt.Errorf("ensure copy for template %q: %w", tmpl.GetName(), err))
		}
	}

	// 2. Prune: delete copies this Secret owns whose source template is gone.
	copies := &unstructured.UnstructuredList{}
	copies.SetGroupVersionKind(publishedResourceListGVK)
	if err := c.List(ctx, copies,
		client.InNamespace(secret.Namespace),
		client.MatchingLabels{agentNameLabel: agentName},
	); err != nil {
		return utilerrors.NewAggregate(append(errs, fmt.Errorf("list owned copies: %w", err)))
	}
	for i := range copies.Items {
		cp := &copies.Items[i]
		src := cp.GetLabels()[pubResTemplateSourceLabel]
		if src == "" {
			continue
		}
		if _, stillHasTemplate := templateNames[src]; stillHasTemplate {
			continue
		}
		if !ownedByUID(cp, secret.UID) {
			continue
		}
		if err := c.Delete(ctx, cp); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete orphan copy %q: %w", cp.GetName(), err))
		}
	}
	return utilerrors.NewAggregate(errs)
}

func ensurePublishedResourceCopy(ctx context.Context, c client.Client, secret *corev1.Secret, agentName string, tmpl *unstructured.Unstructured) error {
	copyName := fmt.Sprintf("%s-%s", tmpl.GetName(), agentName)

	pr := &unstructured.Unstructured{}
	pr.SetGroupVersionKind(publishedResourceGVK)
	pr.SetNamespace(secret.Namespace)
	pr.SetName(copyName)

	_, err := controllerutil.CreateOrUpdate(ctx, c, pr, func() error {
		// Carry the template's spec verbatim.
		spec, _, err := unstructured.NestedFieldCopy(tmpl.Object, "spec")
		if err != nil {
			return fmt.Errorf("read template spec: %w", err)
		}
		if err := unstructured.SetNestedField(pr.Object, spec, "spec"); err != nil {
			return fmt.Errorf("set copy spec: %w", err)
		}

		// Carry labels forward (minus the template marker), stamp our own.
		lbls := map[string]string{}
		for k, v := range tmpl.GetLabels() {
			if k == pubResTemplateLabel {
				continue
			}
			lbls[k] = v
		}
		lbls[managedByLabel] = managedByValue
		lbls[agentNameLabel] = agentName
		lbls[pubResTemplateSourceLabel] = tmpl.GetName()
		pr.SetLabels(lbls)

		return controllerutil.SetControllerReference(secret, pr, c.Scheme())
	})
	return err
}

func ownedByUID(obj client.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
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
				"publishedResourceSelector":  fmt.Sprintf("%s=%s", agentNameLabel, agentName),
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
