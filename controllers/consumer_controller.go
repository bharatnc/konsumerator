/*

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
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"

	konsumeratorv1alpha1 "github.com/lwolf/konsumerator/api/v1alpha1"
	"github.com/lwolf/konsumerator/pkg/errors"
	"github.com/lwolf/konsumerator/pkg/helpers"
	"github.com/lwolf/konsumerator/pkg/providers"
)

var (
	managedPartitionAnnotation  = "konsumerator.lwolf.org/partition"
	disableAutoscalerAnnotation = "konsumerator.lwolf.org/disable-autoscaler"
	deployOwnerKey              = ".metadata.controller"
	apiGVStr                    = konsumeratorv1alpha1.GroupVersion.String()
)

func shouldUpdateLag(consumer *konsumeratorv1alpha1.Consumer) bool {
	status := consumer.Status
	if status.LastSyncTime == nil || status.PartitionsLag == nil {
		return true
	}
	timeToSync := metav1.Now().Sub(status.LastSyncTime.Time) > consumer.Spec.Autoscaler.LagSyncPeriod.Duration
	if len(*status.PartitionsLag) > 0 && timeToSync {
		return true
	}
	return false
}

// ConsumerReconciler reconciles a Consumer object
type ConsumerReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=konsumerator.lwolf.org,resources=consumers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=konsumerator.lwolf.org,resources=consumers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;update;patch;delete;list
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get

func (r *ConsumerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("consumer", req.NamespacedName)

	var consumer konsumeratorv1alpha1.Consumer
	if err := r.Get(ctx, req.NamespacedName, &consumer); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, errors.IgnoreNotFound(err)
	}
	var managedDeploys v1.DeploymentList
	if err := r.List(ctx, &managedDeploys, client.InNamespace(req.Namespace), client.MatchingField(deployOwnerKey, req.Name)); err != nil {
		log.Error(err, "unable to list managed deployments")
		return ctrl.Result{}, err
	}

	var lagProvider providers.LagSource
	// set lagProvider to dummy if disable-autoscaler-annotation is set
	if len(consumer.Annotations[disableAutoscalerAnnotation]) > 0 {
		lagProvider = providers.NewLagSourceDummy(false, *consumer.Spec.NumPartitions)
	} else {
		switch consumer.Spec.Autoscaler.Provider {
		case konsumeratorv1alpha1.LagProviderTypePrometheus:
			lagProvider = providers.NewLagSourcePrometheus(consumer.Spec.Autoscaler.PrometheusProvider)
		case konsumeratorv1alpha1.LagProviderTypeDummy:
			lagProvider = providers.NewLagSourceDummy(false, *consumer.Spec.NumPartitions)
		default:
			lagProvider = providers.NewLagSourceDummy(false, *consumer.Spec.NumPartitions)
		}
		if shouldUpdateLag(&consumer) {
			log.Info("going to upgrade lag")
			if err := lagProvider.Query(); err != nil {
				log.Error(err, "failed to query lag provider")
			}
			lagLst := lagProvider.GetLag()
			tm := metav1.Now()
			consumer.Status.LastSyncTime = &tm
			consumer.Status.PartitionsLag = &lagLst
		} else {
			lagProvider = providers.NewLagSourceKube(*consumer.Status.PartitionsLag)
		}
	}

	var missingPartitions []int32
	var runningInstances []*appsv1.Deployment
	var laggingInstances []*appsv1.Deployment
	var redundantInstances []*appsv1.Deployment

	parts := make(map[int32]bool)
	for _, deploy := range managedDeploys.Items {
		partition := helpers.ParsePartitionAnnotation(deploy.Annotations[managedPartitionAnnotation])
		if partition == nil {
			log.Error(nil, "failed to parse annotation with partition number. Panic!!!")
			continue
		}
		parts[*partition] = true
		lag := lagProvider.GetLagByPartition(*partition)
		runningInstances = append(runningInstances, &deploy)
		if consumer.Spec.Autoscaler.MaxAllowedLag != nil && lag >= int64(consumer.Spec.Autoscaler.MaxAllowedLag.Seconds()) {
			laggingInstances = append(laggingInstances, &deploy)
		}
		if *partition > *consumer.Spec.NumPartitions {
			redundantInstances = append(redundantInstances, &deploy)
		}
	}
	for i := int32(0); i < *consumer.Spec.NumPartitions; i++ {
		if _, ok := parts[i]; !ok {
			missingPartitions = append(missingPartitions, i)
		}
	}
	consumer.Status.Running = helpers.Ptr2Int32(int32(len(runningInstances)))
	consumer.Status.Lagging = helpers.Ptr2Int32(int32(len(laggingInstances)))
	consumer.Status.Expected = consumer.Spec.NumPartitions
	log.V(1).Info("deployments count", "expected", consumer.Spec.NumPartitions, "running", len(runningInstances), "missing", len(missingPartitions), "lagging", len(laggingInstances))

	if err := r.Status().Update(ctx, &consumer); errors.IgnoreConflict(err) != nil {
		log.Error(err, "unable to update Consumer status")
		return ctrl.Result{}, err
	}

	for _, mp := range missingPartitions {
		d, err := r.constructDeployment(consumer, mp)
		if err != nil {
			log.Error(err, "failed to construct deployment from template")
			continue
		}
		if err := r.Create(ctx, d); errors.IgnoreAlreadyExists(err) != nil {
			log.Error(err, "unable to create new Deployment", "deployment", d, "partition", mp)
			continue
		}
		log.V(1).Info("created new Deployment", "deployment", d, "partition", mp)
	}

	if len(redundantInstances) > 0 {
		for _, deploy := range redundantInstances {
			if err := r.Delete(ctx, deploy); errors.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete deployment", "deployment", deploy)
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *ConsumerReconciler) constructDeployment(consumer konsumeratorv1alpha1.Consumer, p int32) (*appsv1.Deployment, error) {
	name := fmt.Sprintf("%s-%d", consumer.Spec.Name, p)
	deploy := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        name,
			Namespace:   consumer.Spec.Namespace,
		},
		Spec: consumer.Spec.DeploymentTemplate,
	}
	deploy.Annotations[managedPartitionAnnotation] = strconv.Itoa(int(p))
	if err := ctrl.SetControllerReference(&consumer, deploy, r.Scheme); err != nil {
		return nil, err
	}
	return deploy, nil
}

func (r *ConsumerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(&appsv1.Deployment{}, deployOwnerKey, func(rawObj runtime.Object) []string {
		// grab the object, extract the owner...
		d := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(d)
		if owner == nil {
			return nil
		}
		if owner.APIVersion != apiGVStr || owner.Kind != "Consumer" {
			return nil
		}
		return []string{owner.Name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&konsumeratorv1alpha1.Consumer{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
