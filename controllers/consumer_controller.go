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
	"strconv"
	"time"

	"github.com/go-logr/logr"

	"github.com/mitchellh/hashstructure"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	konsumeratorv1alpha1 "github.com/lwolf/konsumerator/api/v1alpha1"
	"github.com/lwolf/konsumerator/pkg/errors"
	"github.com/lwolf/konsumerator/pkg/helpers"
	"github.com/lwolf/konsumerator/pkg/predictors"
	"github.com/lwolf/konsumerator/pkg/providers"
)

var (
	managedPartitionAnnotation  = "konsumerator.lwolf.org/partition"
	disableAutoscalerAnnotation = "konsumerator.lwolf.org/disable-autoscaler"
	generationKey               = "konsumerator.lwolf.org/generation"
	ownerKey                    = ".metadata.controller"
	defaultPartitionEnvKey      = "KONSUMERATOR_PARTITION"
	gomaxprocsEnvKey            = "GOMAXPROCS"
	defaultMinSyncPeriod        = time.Minute
	apiGVStr                    = konsumeratorv1alpha1.GroupVersion.String()
)

func shouldUpdateMetrics(consumer *konsumeratorv1alpha1.Consumer) bool {
	status := consumer.Status
	if status.LastSyncTime == nil || status.LastSyncState == nil {
		return true
	}
	timeToSync := metav1.Now().Sub(status.LastSyncTime.Time) > consumer.Spec.Autoscaler.Prometheus.MinSyncPeriod.Duration
	if timeToSync {
		return true
	}
	return false
}

// ConsumerReconciler reconciles a Consumer object
type ConsumerReconciler struct {
	client.Client
	Log      logr.Logger
	recorder record.EventRecorder
	Scheme   *runtime.Scheme
}

// +kubebuilder:rbac:groups=konsumerator.lwolf.org,resources=consumers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=konsumerator.lwolf.org,resources=consumers/status,verbs=get;update;patch

func (r *ConsumerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("consumer", req.NamespacedName)
	result := ctrl.Result{RequeueAfter: defaultMinSyncPeriod}

	var consumer konsumeratorv1alpha1.Consumer
	if err := r.Get(ctx, req.NamespacedName, &consumer); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, errors.IgnoreNotFound(err)
	}
	var managedInstances v1.ReplicaSetList
	if err := r.List(ctx, &managedInstances, client.InNamespace(req.Namespace), client.MatchingField(ownerKey, req.Name)); err != nil {
		eMsg := "unable to list managed ReplicaSets"
		log.Error(err, eMsg)
		r.recorder.Event(&consumer, corev1.EventTypeWarning, "ListReplicaSetFailure", eMsg)
		return ctrl.Result{}, err
	}
	var err error
	var mp providers.MetricsProvider

	_, autoscalerDisabled := consumer.Annotations[disableAutoscalerAnnotation]
	if !autoscalerDisabled && consumer.Spec.Autoscaler != nil {
		switch consumer.Spec.Autoscaler.Mode {
		case konsumeratorv1alpha1.AutoscalerTypePrometheus:
			// setup prometheus metrics provider
			mp, err = providers.NewPrometheusMP(log, consumer.Spec.Autoscaler.Prometheus)
			if err != nil {
				log.Error(err, "failed to initialize Prometheus Metrics Provider")
				break
			}
			providers.LoadSyncState(mp, consumer.Status)
			if shouldUpdateMetrics(&consumer) {
				log.Info("going to update metrics info")
				if err := mp.Update(); err != nil {
					log.Error(err, "failed to query lag provider")
				} else {
					tm := metav1.Now()
					consumer.Status.LastSyncTime = &tm
					consumer.Status.LastSyncState = providers.DumpSyncState(*consumer.Spec.NumPartitions, mp)
				}
			}
		default:
		}
	}
	if mp == nil {
		mp = providers.NewDummyMP(*consumer.Spec.NumPartitions)
	}

	var missingIds []int32
	var pausedIds []int32
	var observedIds []int32
	var laggingIds []int32
	var outdatedIds []int32
	var redundantInstances []*appsv1.ReplicaSet
	var outdatedInstances []*appsv1.ReplicaSet

	hash, err := hashstructure.Hash(consumer.Spec.PodSpec, nil)
	if err != nil {
		return ctrl.Result{}, err
	}
	consumer.Status.ObservedGeneration = helpers.Ptr2Int64(int64(hash))

	trackedPartitions := make(map[int32]bool)
	for i := range managedInstances.Items {
		rs := managedInstances.Items[i].DeepCopy()
		var isOutdated bool
		partition := helpers.ParsePartitionAnnotation(rs.Annotations[managedPartitionAnnotation])
		if partition == nil {
			log.Error(nil, "failed to parse annotation with partition number. Panic!!!")
			continue
		}
		trackedPartitions[*partition] = true
		lag := mp.GetLagByPartition(*partition)
		r.Log.Info("lag per partition", "partition", *partition, "lag", lag)
		if rs.Status.Replicas > 0 {
			observedIds = append(observedIds, *partition)
		} else {
			pausedIds = append(pausedIds, *partition)
		}
		if *partition >= *consumer.Spec.NumPartitions {
			redundantInstances = append(redundantInstances, rs)
			continue
		}
		if consumer.Spec.Autoscaler.Prometheus.TolerableLag != nil && lag >= consumer.Spec.Autoscaler.Prometheus.TolerableLag.Duration {
			isOutdated = true
			laggingIds = append(laggingIds, *partition)
		}
		if rs.Annotations[generationKey] != strconv.Itoa(int(*consumer.Status.ObservedGeneration)) {
			isOutdated = true
			outdatedIds = append(outdatedIds, *partition)
		}
		if isOutdated {
			outdatedInstances = append(outdatedInstances, rs)
		}
	}
	for i := int32(0); i < *consumer.Spec.NumPartitions; i++ {
		if _, ok := trackedPartitions[i]; !ok {
			missingIds = append(missingIds, i)
		}
	}
	consumer.Status.Running = helpers.Ptr2Int32(int32(len(observedIds)))
	consumer.Status.Lagging = helpers.Ptr2Int32(int32(len(laggingIds)))
	consumer.Status.Outdated = helpers.Ptr2Int32(int32(len(outdatedInstances)))
	consumer.Status.Expected = consumer.Spec.NumPartitions
	log.V(1).Info(
		"instances count",
		"expected", consumer.Spec.NumPartitions,
		"running", len(observedIds),
		"paused", len(pausedIds),
		"missing", len(missingIds),
		"lagging", len(laggingIds),
		"outdated", len(outdatedInstances),
	)

	if err := r.Status().Update(ctx, &consumer); errors.IgnoreConflict(err) != nil {
		eMsg := "unable to update Consumer status"
		log.Error(err, eMsg)
		r.recorder.Event(&consumer, corev1.EventTypeWarning, "UpdateConsumerStatus", eMsg)
		return result, err
	}

	for _, part := range missingIds {
		d, err := r.constructReplicaSet(consumer, part, mp)
		if err != nil {
			log.Error(err, "failed to construct replicaSet from template")
			continue
		}
		if err := r.Create(ctx, d); errors.IgnoreAlreadyExists(err) != nil {
			log.Error(err, "unable to create new ReplicaSet", "replicaSet", d, "partition", part)
			continue
		}
		log.V(1).Info("created new replicaSet", "replicaSet", d, "partition", part)
		r.recorder.Eventf(
			&consumer,
			corev1.EventTypeNormal,
			"ReplicaSetCreate",
			"replicaSet for partition was created %d", part,
		)
	}

	for _, deploy := range redundantInstances {
		if err := r.Delete(ctx, deploy); errors.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to delete replicaSet", "replicaSet", deploy)
		}
		r.recorder.Eventf(
			&consumer,
			corev1.EventTypeNormal,
			"ReplicaSetDelete",
			"replicaSet %s was deleted", deploy.Name,
		)
	}

	var partitionKey string
	if consumer.Spec.PartitionEnvKey != "" {
		partitionKey = consumer.Spec.PartitionEnvKey
	} else {
		partitionKey = defaultPartitionEnvKey
	}

	for _, rs := range outdatedInstances {
		labels := instanceLabels(consumer.Name)
		rs.Annotations[generationKey] = strconv.Itoa(int(*consumer.Status.ObservedGeneration))
		rs.Spec = appsv1.ReplicaSetSpec{
			Replicas: helpers.Ptr2Int32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: consumer.Spec.PodSpec,
			},
		}
		partition := helpers.ParsePartitionAnnotation(rs.Annotations[managedPartitionAnnotation])
		if partition == nil {
			log.Error(nil, "failed to parse annotation with partition number.")
			continue
		}
		for containerIndex := range rs.Spec.Template.Spec.Containers {
			name := rs.Spec.Template.Spec.Containers[containerIndex].Name
			// TODO: do not call predictors at all if providers.DummyMP is used
			// TODO: make predictors configurable
			predictor := predictors.NewNaivePredictor(log, mp, consumer.Spec.Autoscaler.Prometheus)
			limits := predictors.GetResourcePolicy(name, &consumer.Spec)
			resources := predictor.Estimate(name, limits, *partition)
			rs.Spec.Template.Spec.Containers[containerIndex].Resources = *resources
			envs := rs.Spec.Template.Spec.Containers[containerIndex].Env
			envs = setEnvVariable(envs, partitionKey, strconv.Itoa(int(*partition)))
			envs = setEnvVariable(envs, gomaxprocsEnvKey, goMaxProcsFromResource(resources.Limits.Cpu()))
			rs.Spec.Template.Spec.Containers[containerIndex].Env = envs
			r.recorder.Eventf(
				&consumer,
				corev1.EventTypeNormal,
				"ReplicaSetUpdate",
				"replicaSet=%s, container=%s, CpuReq=%d, CpuLimit=%d",
				rs.Name, name, resources.Requests.Cpu().MilliValue(), resources.Limits.Cpu().MilliValue(),
			)
		}
		if err := r.Update(ctx, rs); errors.IgnoreConflict(err) != nil {
			log.Error(err, "unable to update replicaSet", "replicaSet", rs)
			r.recorder.Eventf(
				&consumer,
				corev1.EventTypeWarning,
				"ReplicaSetUpdate",
				"replicaSet %s: update failed", rs.Name,
			)
			continue
		}
		r.recorder.Eventf(
			&consumer,
			corev1.EventTypeNormal,
			"ReplicaSetUpdate",
			"replicaSet %s: updated", rs.Name,
		)

	}
	return result, nil
}

func goMaxProcsFromResource(cpu *resource.Quantity) string {
	value := int(cpu.Value())
	if value < 1 {
		value = 1
	}
	return strconv.Itoa(value)
}

func setEnvVariable(envVars []corev1.EnvVar, key string, value string) []corev1.EnvVar {
	envs := make([]corev1.EnvVar, len(envVars))
	copy(envs, envVars)
	for i, e := range envs {
		if e.Name == key {
			envs[i].Value = value
			return envs
		}
	}
	envs = append(envs, corev1.EnvVar{
		Name:  key,
		Value: value,
	})
	return envs
}

func instanceLabels(consumerName string) map[string]string {
	return map[string]string{
		"app": consumerName,
	}
}

func (r *ConsumerReconciler) constructReplicaSet(consumer konsumeratorv1alpha1.Consumer, partition int32, store providers.MetricsProvider) (*appsv1.ReplicaSet, error) {
	labels := instanceLabels(consumer.Name)
	annotations := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "9000",
	}
	rs := &appsv1.ReplicaSet{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: annotations,
			Name:        fmt.Sprintf("%s-%d", consumer.Spec.Name, partition),
			Namespace:   consumer.Spec.Namespace,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: helpers.Ptr2Int32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: consumer.Spec.PodSpec,
			},
		},
	}
	rs.Annotations[managedPartitionAnnotation] = strconv.Itoa(int(partition))
	rs.Annotations[generationKey] = strconv.Itoa(int(*consumer.Status.ObservedGeneration))
	var partitionKey string
	if consumer.Spec.PartitionEnvKey != "" {
		partitionKey = consumer.Spec.PartitionEnvKey
	} else {
		partitionKey = defaultPartitionEnvKey
	}
	for containerIndex := range rs.Spec.Template.Spec.Containers {
		name := rs.Spec.Template.Spec.Containers[containerIndex].Name
		predictor := predictors.NewNaivePredictor(r.Log, store, consumer.Spec.Autoscaler.Prometheus)
		limits := predictors.GetResourcePolicy(name, &consumer.Spec)
		resources := predictor.Estimate(name, limits, partition)
		rs.Spec.Template.Spec.Containers[containerIndex].Resources = *resources
		envs := rs.Spec.Template.Spec.Containers[containerIndex].Env
		envs = setEnvVariable(envs, partitionKey, strconv.Itoa(int(partition)))
		envs = setEnvVariable(envs, gomaxprocsEnvKey, goMaxProcsFromResource(resources.Limits.Cpu()))
		rs.Spec.Template.Spec.Containers[containerIndex].Env = envs
	}
	if err := ctrl.SetControllerReference(&consumer, rs, r.Scheme); err != nil {
		return nil, err
	}
	return rs, nil
}

func (r *ConsumerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(&appsv1.ReplicaSet{}, ownerKey, func(rawObj runtime.Object) []string {
		// grab the object, extract the owner...
		d := rawObj.(*appsv1.ReplicaSet)
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
	r.recorder = mgr.GetEventRecorderFor("konsumerator")
	return ctrl.NewControllerManagedBy(mgr).
		For(&konsumeratorv1alpha1.Consumer{}).
		Owns(&appsv1.ReplicaSet{}).
		Complete(r)
}
