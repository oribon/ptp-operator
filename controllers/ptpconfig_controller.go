package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/golang/glog"
	ptpv1 "github.com/openshift/ptp-operator/api/v1"
	"github.com/openshift/ptp-operator/pkg/names"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type PtpConfigReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a PtpConfig object and makes changes based on the state read
// and what is in the PtpConfig.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *PtpConfigReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := r.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling PtpConfig")

	instances := &ptpv1.PtpConfigList{}
	err := r.List(context.TODO(), instances, &client.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	nodeList := &corev1.NodeList{}
	err = r.List(context.TODO(), nodeList, &client.ListOptions{})
	if err != nil {
		glog.Errorf("failed to list nodes")
		return reconcile.Result{}, err
	}

	if err = r.syncPtpConfig(instances, nodeList); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// syncPtpConfig synchronizes PtpConfig CR
func (r *PtpConfigReconciler) syncPtpConfig(ptpConfigList *ptpv1.PtpConfigList, nodeList *corev1.NodeList) error {
	var err error

	nodePtpConfigMap := &corev1.ConfigMap{}
	nodePtpConfigMap.Name = names.DefaultPTPConfigMapName
	nodePtpConfigMap.Namespace = names.Namespace
	nodePtpConfigMap.Data = make(map[string]string)

	for _, node := range nodeList.Items {
		nodePtpProfiles, err := getRecommendNodePtpProfiles(ptpConfigList, node)
		if err != nil {
			return fmt.Errorf("failed to get recommended node PtpConfig: %v", err)
		}

		data, err := json.Marshal(nodePtpProfiles)
		if err != nil {
			return fmt.Errorf("failed to Marshal nodePtpProfiles: %v", err)
		}
		nodePtpConfigMap.Data[node.Name] = string(data)
	}

	cm := &corev1.ConfigMap{}
	err = r.Get(context.TODO(), types.NamespacedName{
		Namespace: names.Namespace, Name: names.DefaultPTPConfigMapName}, cm)
	if err != nil {
		return fmt.Errorf("failed to get ptp config map: %v", err)
	} else {
		glog.Infof("ptp config map already exists, updating")
		cm.Data = nodePtpConfigMap.Data
		err = r.Update(context.TODO(), cm)
		if err != nil {
			return fmt.Errorf("failed to update ptp config map: %v", err)
		}
	}
	return nil
}

func (r *PtpConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ptpv1.PtpConfig{}).
		Complete(r)
}
