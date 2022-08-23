/*
Copyright 2022.

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
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	client2 "github.com/grafana-operator/grafana-operator-experimental/controllers/client"
	gapi "github.com/grafana/grafana-api-golang-client"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	grafanav1beta1 "github.com/grafana-operator/grafana-operator-experimental/api/v1beta1"
)

// GrafanaDatasourceReconciler reconciles a GrafanaDatasource object
type GrafanaDatasourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadatasources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadatasources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadatasources/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the GrafanaDatasource object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.1/pkg/reconcile
func (r *GrafanaDatasourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controllerLog := log.FromContext(ctx)
	r.Log = controllerLog

	datasource := &grafanav1beta1.GrafanaDatasource{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, datasource)

	if err != nil {
		controllerLog.Error(err, "error getting grafana dashboard cr")
		return ctrl.Result{RequeueAfter: RequeueDelayError}, err
	}

	if datasource.Spec.InstanceSelector == nil {
		controllerLog.Info("no instance selector found for datasource, nothing to do", "name", datasource.Name, "namespace", datasource.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, err
	}

	instances, err := GetMatchingInstances(ctx, r.Client, datasource.Spec.InstanceSelector)
	if err != nil {
		controllerLog.Error(err, "could not find matching instance", "name", datasource.Name)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, err
	}

	if len(instances.Items) == 0 {
		controllerLog.Info("no matching instances found for datasource", "datasource", datasource.Name, "namespace", datasource.Namespace)
	}

	controllerLog.Info("found matching Grafana instances for datasource", "count", len(instances.Items))

	for _, grafana := range instances.Items {
		// an admin url is required to interact with grafana
		// the instance or route might not yet be ready
		if grafana.Status.AdminUrl == "" || grafana.Status.Stage != grafanav1beta1.OperatorStageComplete || grafana.Status.StageStatus != grafanav1beta1.OperatorStageResultSuccess {
			controllerLog.Info("grafana instance not ready", "grafana", grafana.Name)
			continue
		}

		// first reconcile the plugins
		// append the requested dashboards to a configmap from where the
		// grafana reconciler will pick them upi
		err = ReconcilePlugins(ctx, r.Client, r.Scheme, &grafana, datasource.Spec.Plugins, fmt.Sprintf("%v-datasource", datasource.Name))
		if err != nil {
			controllerLog.Error(err, "error reconciling plugins", "datasource", datasource.Name, "grafana", grafana.Name)
		}

		// then import the dashboard into the matching grafana instances
		err = r.onDatasourceCreated(ctx, &grafana, datasource)
		if err != nil {
			controllerLog.Error(err, "error reconciling dashboard", "datasource", datasource.Name, "grafana", grafana.Name)
		}
	}

	return ctrl.Result{RequeueAfter: RequeueDelaySuccess}, nil

}

func (r *GrafanaDatasourceReconciler) onDatasourceCreated(ctx context.Context, grafana *grafanav1beta1.Grafana, cr *grafanav1beta1.GrafanaDatasource) error {
	if cr.Spec.Datasource == nil {
		return nil
	}

	grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, grafana)
	if err != nil {
		return err
	}

	id, err := r.ExistingId(grafanaClient, cr)
	if err != nil {
		return err
	}

	// always use the same uid for CR and datasource
	cr.Spec.Datasource.UID = string(cr.UID)
	datasourceBytes, err := json.Marshal(cr.Spec.Datasource)
	if err != nil {
		return err
	}

	if id == nil {
		_, err = grafanaClient.NewDataSourceFromRawData(datasourceBytes)
		if err != nil {
			return err
		}
	} else if cr.Unchanged() == false {
		err := grafanaClient.UpdateDataSourceFromRawData(*id, datasourceBytes)
		if err != nil {
			return err
		}
	} else {
		// datasource exists and is unchanged, nothing to do
		return nil
	}

	err = r.UpdateStatus(ctx, cr)
	if err != nil {
		return err
	}

	return grafana.AddDatasource(cr.Namespace, cr.Name, string(cr.UID))
}

func (r *GrafanaDatasourceReconciler) UpdateStatus(ctx context.Context, cr *grafanav1beta1.GrafanaDatasource) error {
	cr.Status.Hash = cr.Hash()
	return r.Client.Status().Update(ctx, cr)
}

func (r *GrafanaDatasourceReconciler) ExistingId(client *gapi.Client, cr *grafanav1beta1.GrafanaDatasource) (*int64, error) {
	datasources, err := client.DataSources()
	if err != nil {
		return nil, err
	}
	for _, datasource := range datasources {
		if datasource.UID == string(cr.UID) {
			return &datasource.ID, nil
		}
	}
	return nil, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GrafanaDatasourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&grafanav1beta1.GrafanaDatasource{}).
		Complete(r)
}