/*
Copyright 2021.

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

package daemon

import (
	"context"
	"flag"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang/glog"
	ptpv1 "github.com/openshift/ptp-operator/api/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// PtpOperatorConfigReconciler reconciles a PtpOperatorConfig object
type PtpOperatorConfigReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

const (
	ResyncPeriod = 2 * time.Minute
)

//+kubebuilder:rbac:groups=ptp.openshift.io,resources=ptpoperatorconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ptp.openshift.io,resources=ptpoperatorconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ptp.openshift.io,resources=ptpoperatorconfigs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PtpOperatorConfig object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *PtpOperatorConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcile.Result, error) {
	reqLogger := r.Log.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)
	reqLogger.Info("Reconciling PtpOperatorConfig")

	cfg := &ptpv1.PtpOperatorConfig{}
	err := r.Get(context.TODO(), req.NamespacedName, cfg)
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, err
	}

	var logLevel = glog.Level(cfg.Spec.LogLevel)
	if logLevel != flag.Lookup("v").Value.(flag.Getter).Get() {
		glog.Infof("Set log verbose level to: %d", logLevel)
		flag.Set("v", logLevel.String())
	}

	return reconcile.Result{RequeueAfter: ResyncPeriod}, nil
}

func (r *PtpOperatorConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ptpv1.PtpOperatorConfig{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				_, ok := e.Object.(*ptpv1.PtpOperatorConfig)
				return ok
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				old, _ := e.ObjectOld.(*ptpv1.PtpOperatorConfig)
				new, ok := e.ObjectNew.(*ptpv1.PtpOperatorConfig)
				if !ok {
					return false
				}

				return old.Spec.LogLevel != new.Spec.LogLevel
			},
		}).
		Complete(r)
}
