/*
Copyright 2026 The Tekton Authors

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

package syncerservice

import (
	"context"
	"errors"
	"fmt"

	mf "github.com/manifestival/manifestival"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	clientset "github.com/tektoncd/operator/pkg/client/clientset/versioned"
	operatorv1alpha1 "github.com/tektoncd/operator/pkg/client/informers/externalversions/operator/v1alpha1"
	syncerservicereconciler "github.com/tektoncd/operator/pkg/client/injection/reconciler/operator/v1alpha1/syncerservice"
	"github.com/tektoncd/operator/pkg/reconciler/common"
	"github.com/tektoncd/operator/pkg/reconciler/kubernetes/tektoninstallerset"
	"github.com/tektoncd/operator/pkg/reconciler/kubernetes/tektoninstallerset/client"
	"github.com/tektoncd/operator/pkg/reconciler/shared/hash"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
)

// Reconciler implements controller.Reconciler for SyncerService resources.
type Reconciler struct {
	kubeClientSet      kubernetes.Interface
	operatorClientSet  clientset.Interface
	installerSetClient *client.InstallerSetClient
	manifest           mf.Manifest
	extension          common.Extension
	pipelineInformer   operatorv1alpha1.TektonPipelineInformer
	operatorVersion    string
	syncerVersion      string
}

// Check that our Reconciler implements controller.Reconciler
var _ syncerservicereconciler.Interface = (*Reconciler)(nil)
var _ syncerservicereconciler.Finalizer = (*Reconciler)(nil)

const createdByValue = "SyncerService"

var (
	ls = metav1.LabelSelector{
		MatchLabels: map[string]string{
			v1alpha1.CreatedByKey:     createdByValue,
			v1alpha1.InstallerSetType: v1alpha1.SyncerServiceResourceName,
		},
	}
)

// FinalizeKind removes all resources after deletion of a SyncerService.
func (r *Reconciler) FinalizeKind(ctx context.Context, original *v1alpha1.SyncerService) pkgreconciler.Event {
	logger := logging.FromContext(ctx)

	labelSelector, err := common.LabelSelector(ls)
	if err != nil {
		return err
	}
	if err := r.operatorClientSet.OperatorV1alpha1().TektonInstallerSets().
		DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: labelSelector,
		}); err != nil {
		logger.Error("Failed to delete installer set created by SyncerService", err)
		return err
	}

	if err := r.extension.Finalize(ctx, original); err != nil {
		logger.Error("Failed to finalize platform resources", err)
	}

	return nil
}

// ReconcileKind compares the actual state with the desired, and attempts to
// converge the two.
func (r *Reconciler) ReconcileKind(ctx context.Context, ss *v1alpha1.SyncerService) pkgreconciler.Event {
	logger := logging.FromContext(ctx).With("syncerservice", ss.Name)

	ss.Status.InitializeConditions()
	ss.Status.ObservedGeneration = ss.Generation

	logger.Infow("Starting SyncerService reconciliation",
		"version", r.syncerVersion,
		"generation", ss.Generation)

	if ss.GetName() != v1alpha1.SyncerServiceResourceName {
		msg := fmt.Sprintf("Resource ignored, Expected Name: %s, Got Name: %s",
			v1alpha1.SyncerServiceResourceName, ss.GetName())
		ss.Status.MarkNotReady(msg)
		return nil
	}

	// Check for TektonPipeline dependency
	tp, err := common.PipelineReady(r.pipelineInformer)
	if err != nil {
		if err.Error() == common.PipelineNotReady || err == v1alpha1.DEPENDENCY_UPGRADE_PENDING_ERR {
			ss.Status.MarkDependencyInstalling("tekton-pipelines is still installing")
			return fmt.Errorf(common.PipelineNotReady)
		}
		ss.Status.MarkDependencyMissing("tekton-pipelines does not exist")
		return err
	}

	if tp.GetSpec().GetTargetNamespace() != ss.GetSpec().GetTargetNamespace() {
		errMsg := fmt.Sprintf("tekton-pipelines is missing in %s namespace", ss.GetSpec().GetTargetNamespace())
		ss.Status.MarkDependencyMissing(errMsg)
		return errors.New(errMsg)
	}

	ss.Status.MarkDependenciesInstalled()

	// reconcile target namespace
	if err := common.ReconcileTargetNamespace(ctx, nil, nil, ss, r.kubeClientSet); err != nil {
		return err
	}

	if err := r.installerSetClient.RemoveObsoleteSets(ctx); err != nil {
		logger.Error("failed to remove obsolete installer sets: %v", err)
		return err
	}

	if err := r.extension.PreReconcile(ctx, ss); err != nil {
		if err == v1alpha1.REQUEUE_EVENT_AFTER {
			return err
		}
		msg := fmt.Sprintf("PreReconciliation failed: %s", err.Error())
		ss.Status.MarkPreReconcilerFailed(msg)
		return nil
	}

	ss.Status.MarkPreReconcilerComplete()

	// Create/Update InstallerSet
	if err := r.ensureInstallerSet(ctx, ss); err != nil {
		return err
	}

	if err := r.extension.PostReconcile(ctx, ss); err != nil {
		if err == v1alpha1.REQUEUE_EVENT_AFTER {
			return err
		}
		msg := fmt.Sprintf("PostReconciliation failed: %s", err.Error())
		ss.Status.MarkPostReconcilerFailed(msg)
		return nil
	}

	ss.Status.MarkPostReconcilerComplete()

	logger.Infow("SyncerService reconciliation completed successfully",
		"ready", ss.Status.IsReady())

	return nil
}

func (r *Reconciler) ensureInstallerSet(ctx context.Context, ss *v1alpha1.SyncerService) error {
	logger := logging.FromContext(ctx)

	labelSelector, err := common.LabelSelector(ls)
	if err != nil {
		return err
	}

	existingInstallerSet, err := tektoninstallerset.CurrentInstallerSetName(ctx, r.operatorClientSet, labelSelector)
	if err != nil {
		return err
	}

	if existingInstallerSet == "" {
		logger.Info("No existing installer set found, creating new one")
		createdIs, err := r.createInstallerSet(ctx, ss)
		if err != nil {
			return err
		}
		r.updateStatus(ss, createdIs)
		return v1alpha1.REQUEUE_EVENT_AFTER
	}

	installedTIS, err := r.operatorClientSet.OperatorV1alpha1().TektonInstallerSets().
		Get(ctx, existingInstallerSet, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			createdIs, err := r.createInstallerSet(ctx, ss)
			if err != nil {
				return err
			}
			r.updateStatus(ss, createdIs)
			return v1alpha1.REQUEUE_EVENT_AFTER
		}
		return err
	}

	installerSetTargetNamespace := installedTIS.Annotations[v1alpha1.TargetNamespaceKey]
	installerSetReleaseVersion := installedTIS.Labels[v1alpha1.ReleaseVersionKey]

	if installerSetTargetNamespace != ss.Spec.TargetNamespace || installerSetReleaseVersion != r.operatorVersion {
		logger.Infow("Configuration changed, deleting existing installer set",
			"existingNamespace", installerSetTargetNamespace,
			"newNamespace", ss.Spec.TargetNamespace)
		err := r.operatorClientSet.OperatorV1alpha1().TektonInstallerSets().
			Delete(ctx, existingInstallerSet, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		ss.Status.MarkNotReady("Waiting for previous installer set to get deleted")
		return v1alpha1.REQUEUE_EVENT_AFTER
	}

	// Check spec hash
	expectedSpecHash, err := hash.Compute(ss.Spec)
	if err != nil {
		return err
	}

	lastAppliedHash := installedTIS.GetAnnotations()[v1alpha1.LastAppliedHashKey]
	if lastAppliedHash != expectedSpecHash {
		logger.Infow("SyncerService spec changed, updating installer set")
		manifest := r.manifest
		if err := r.transform(ctx, &manifest, ss); err != nil {
			return err
		}

		current := installedTIS.GetAnnotations()
		current[v1alpha1.LastAppliedHashKey] = expectedSpecHash
		installedTIS.SetAnnotations(current)
		installedTIS.Spec.Manifests = manifest.Resources()

		_, err := r.operatorClientSet.OperatorV1alpha1().TektonInstallerSets().
			Update(ctx, installedTIS, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		return v1alpha1.REQUEUE_EVENT_AFTER
	}

	r.updateStatus(ss, installedTIS)
	ss.Status.MarkInstallerSetAvailable()

	ready := installedTIS.Status.GetCondition(apis.ConditionReady)
	if ready == nil || ready.Status == "Unknown" {
		ss.Status.MarkInstallerSetNotReady("Waiting for installation")
		return v1alpha1.REQUEUE_EVENT_AFTER
	}

	if ready.Status == "False" {
		ss.Status.MarkInstallerSetNotReady(ready.Message)
		return v1alpha1.REQUEUE_EVENT_AFTER
	}

	ss.Status.MarkInstallerSetReady()
	return nil
}

func (r *Reconciler) createInstallerSet(ctx context.Context, ss *v1alpha1.SyncerService) (*v1alpha1.TektonInstallerSet, error) {
	manifest := r.manifest

	if err := r.transform(ctx, &manifest, ss); err != nil {
		return nil, err
	}

	specHash, err := hash.Compute(ss.Spec)
	if err != nil {
		return nil, err
	}

	tis := &v1alpha1.TektonInstallerSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", v1alpha1.SyncerServiceResourceName),
			Labels: map[string]string{
				v1alpha1.CreatedByKey:      createdByValue,
				v1alpha1.InstallerSetType:  v1alpha1.SyncerServiceResourceName,
				v1alpha1.ReleaseVersionKey: r.operatorVersion,
			},
			Annotations: map[string]string{
				v1alpha1.TargetNamespaceKey: ss.Spec.TargetNamespace,
				v1alpha1.LastAppliedHashKey: specHash,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(ss, ss.GetGroupVersionKind()),
			},
		},
		Spec: v1alpha1.TektonInstallerSetSpec{
			Manifests: manifest.Resources(),
		},
	}

	return r.operatorClientSet.OperatorV1alpha1().TektonInstallerSets().Create(ctx, tis, metav1.CreateOptions{})
}

func (r *Reconciler) updateStatus(ss *v1alpha1.SyncerService, installerSet *v1alpha1.TektonInstallerSet) {
	ss.Status.SetSyncerServiceInstallerSet(installerSet.Name)
	ss.Status.SetVersion(r.syncerVersion)
}
