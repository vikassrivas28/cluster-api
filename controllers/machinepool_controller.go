/*
Copyright 2019 The Kubernetes Authors.

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
	"sync"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/remote"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;bootstrap.cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools;machinepools/status,verbs=get;list;watch;create;update;patch;delete

// MachinePoolReconciler reconciles a MachinePool object
type MachinePoolReconciler struct {
	Client client.Client
	Log    logr.Logger

	config           *rest.Config
	controller       controller.Controller
	recorder         record.EventRecorder
	externalWatchers sync.Map
	scheme           *runtime.Scheme
}

func (r *MachinePoolReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.MachinePool{}).
		WithOptions(options).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	r.controller = c
	r.recorder = mgr.GetEventRecorderFor("machinepool-controller")
	r.config = mgr.GetConfig()
	r.scheme = mgr.GetScheme()
	return nil
}

func (r *MachinePoolReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx := context.Background()
	logger := r.Log.WithValues("machinepool", req.NamespacedName)

	mp := &clusterv1.MachinePool{}
	if err := r.Client.Get(ctx, req.NamespacedName, mp); err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return. Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Error reading the object - requeue the request.")
		return ctrl.Result{}, err
	}

	cluster, err := util.GetClusterByName(ctx, r.Client, mp.ObjectMeta.Namespace, mp.Spec.ClusterName)
	if err != nil {
		logger.Error(err, "Failed to get Cluster %s for MachinePool.", mp.Spec.ClusterName)
		return ctrl.Result{}, errors.Wrapf(err, "failed to get cluster %q for machinepool %q in namespace %q",
			mp.Spec.ClusterName, mp.Name, mp.Namespace)
	}

	// Return early if the object or Cluster is paused.
	if util.IsPaused(cluster, mp) {
		logger.V(3).Info("reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(mp, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		r.reconcilePhase(mp)
		// TODO(jpang): add support for metrics.

		// Always attempt to patch the object and status after each reconciliation.
		if err := patchHelper.Patch(ctx, mp); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	// Reconcile labels.
	if mp.Labels == nil {
		mp.Labels = make(map[string]string)
	}
	mp.Labels[clusterv1.ClusterLabelName] = mp.Spec.ClusterName

	// Handle deletion reconciliation loop.
	if !mp.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster, mp)
	}

	// Handle normal reconciliation loop.
	return r.reconcile(ctx, cluster, mp)
}

func (r *MachinePoolReconciler) reconcile(ctx context.Context, cluster *clusterv1.Cluster, mp *clusterv1.MachinePool) (ctrl.Result, error) {
	logger := r.Log.WithValues("machinepool", mp.Name, "namespace", mp.Namespace)
	logger = logger.WithValues("cluster", cluster.Name)

	// Ensure the MachinePool is owned by the Cluster it belongs to.
	mp.OwnerReferences = util.EnsureOwnerRef(mp.OwnerReferences, metav1.OwnerReference{
		APIVersion: cluster.APIVersion,
		Kind:       cluster.Kind,
		Name:       cluster.Name,
		UID:        cluster.UID,
	})

	// If the MachinePool doesn't have a finalizer, add one.
	controllerutil.AddFinalizer(mp, clusterv1.MachinePoolFinalizer)

	// Call the inner reconciliation methods.
	reconciliationErrors := []error{
		r.reconcileBootstrap(ctx, cluster, mp),
		r.reconcileInfrastructure(ctx, cluster, mp),
		r.reconcileNodeRefs(ctx, cluster, mp),
	}

	// Parse the errors, making sure we record if there is a RequeueAfterError.
	res := ctrl.Result{}
	errs := []error{}
	for _, err := range reconciliationErrors {
		if requeueErr, ok := errors.Cause(err).(capierrors.HasRequeueAfterError); ok {
			// Only record and log the first RequeueAfterError.
			if !res.Requeue {
				res.Requeue = true
				res.RequeueAfter = requeueErr.GetRequeueAfter()
				logger.Error(err, "Reconciliation for MachinePool asked to requeue")
			}
			continue
		}

		errs = append(errs, err)
	}
	return res, kerrors.NewAggregate(errs)
}

func (r *MachinePoolReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, mp *clusterv1.MachinePool) (ctrl.Result, error) {
	if ok, err := r.reconcileDeleteExternal(ctx, mp); !ok || err != nil {
		// Return early and don't remove the finalizer if we got an error or
		// the external reconciliation deletion isn't ready.
		return ctrl.Result{}, err
	}

	if err := r.reconcileDeleteNodes(ctx, cluster, mp); err != nil {
		// Return early and don't remove the finalizer if we got an error.
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(mp, clusterv1.MachinePoolFinalizer)
	return ctrl.Result{}, nil
}

func (r *MachinePoolReconciler) reconcileDeleteNodes(ctx context.Context, cluster *clusterv1.Cluster, machinepool *clusterv1.MachinePool) error {
	if len(machinepool.Status.NodeRefs) == 0 {
		return nil
	}

	clusterClient, err := remote.NewClusterClient(ctx, r.Client, util.ObjectKey(cluster), r.scheme)
	if err != nil {
		return err
	}

	if err := r.deleteRetiredNodes(ctx, clusterClient, machinepool.Status.NodeRefs, machinepool.Spec.ProviderIDList); err != nil {
		return err
	}
	return nil
}

// reconcileDeleteExternal tries to delete external references, returning true if it cannot find any.
func (r *MachinePoolReconciler) reconcileDeleteExternal(ctx context.Context, m *clusterv1.MachinePool) (bool, error) {
	objects := []*unstructured.Unstructured{}
	references := []*corev1.ObjectReference{
		m.Spec.Template.Spec.Bootstrap.ConfigRef,
		&m.Spec.Template.Spec.InfrastructureRef,
	}

	// Loop over the references and try to retrieve it with the client.
	for _, ref := range references {
		if ref == nil {
			continue
		}

		obj, err := external.Get(ctx, r.Client, ref, m.Namespace)
		if err != nil && !apierrors.IsNotFound(errors.Cause(err)) {
			return false, errors.Wrapf(err, "failed to get %s %q for MachinePool %q in namespace %q",
				ref.GroupVersionKind(), ref.Name, m.Name, m.Namespace)
		}
		if obj != nil {
			objects = append(objects, obj)
		}
	}

	// Issue a delete request for any object that has been found.
	for _, obj := range objects {
		if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return false, errors.Wrapf(err,
				"failed to delete %v %q for MachinePool %q in namespace %q",
				obj.GroupVersionKind(), obj.GetName(), m.Name, m.Namespace)
		}
	}

	// Return true if there are no more external objects.
	return len(objects) == 0, nil
}
