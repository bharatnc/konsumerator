package predictors

import (
	"fmt"
	"math"

	autoscalev1 "github.com/kubernetes/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const bucketSize = 10

/*
Predictions should be made for 2 distinct cases:
* new deployment: no actual data present, except for prometheus metrics about production rate, consumption rate and lag
* existing rescheduling: data about other deployments along with the metrics are present and could be used
for better estimation of required resources.
*/

// PredictResourcesBasedOnProductionRate produces resource requirements for a all the partitions
// based on the production rate map and resource constrains: minAllowd resources and maxAllowed resources
// minAllowed resources should be set to the minimum resources needed to keep up with the
// the consumption of the least heavy partition
func AssignResourcesBasedOnProductionRate(policy autoscalev1.ContainerResourcePolicy, prodRate map[int32]int64) map[int32]corev1.ResourceRequirements {
	buckets := makeBuckets(findMin(prodRate), findMax(prodRate), bucketSize)
	if len(buckets) == 0 {
		return nil
	}
	resourceBuckets := makeResourceBuckets(policy.MinAllowed, policy.MaxAllowed, bucketSize)
	var resources = map[int32]corev1.ResourceRequirements{}
	for partNum, rate := range prodRate {
		bucketNum, _ := getBucketNum(rate, buckets)
		r := resourceBuckets[bucketNum]
		resources[partNum] = corev1.ResourceRequirements{
			Requests: r,
			Limits:   r,
		}
	}
	return resources
}

func getBucketNum(num int64, buckets []int64) (int64, error) {
	if len(buckets) == 0 {
		return 0, fmt.Errorf("empty buckets list")
	}
	if num >= buckets[len(buckets)-1] {
		return int64(len(buckets) - 1), nil
	}
	for i, b := range buckets {
		if num < b {
			return int64(i), nil
		}
	}
	// this case shouldn't happen, but we will return maximum resources
	// if we weren't able to get bucket
	return int64(len(buckets) - 1), nil
}

func makeResourceBuckets(minRes corev1.ResourceList, maxRes corev1.ResourceList, size int64) []corev1.ResourceList {
	resCPU := minRes.Cpu().DeepCopy()
	resMem := minRes.Memory().DeepCopy()
	resCPU.Add(*maxRes.Cpu())
	resMem.Add(*maxRes.Memory())
	stepCpu := resCPU.MilliValue() / size
	stepMem := resMem.MilliValue() / size
	var result []corev1.ResourceList
	for i := int64(0); i < size; i++ {
		vres := corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(minRes.Cpu().MilliValue()+stepCpu*i, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewMilliQuantity(minRes.Memory().MilliValue()+stepMem*i, resource.DecimalSI),
		}
		result = append(result, vres)
	}
	return result
}

func makeBuckets(minP int64, maxP int64, size int64) []int64 {
	var buckets []int64
	if size == 0 {
		return buckets
	}
	step := (maxP - minP) / size
	if step < 1 {
		step = 1
	}
	for i := int64(0); i < size; i++ {
		value := minP + (step * i)
		if value >= maxP {
			break
		}
		buckets = append(buckets, value)
	}
	return buckets
}

// findMin returns maximum value in the map
func findMin(floats map[int32]int64) int64 {
	min := int64(math.MaxInt64)
	for i := range floats {
		if floats[i] < min {
			min = floats[i]
		}
	}
	return min
}

// findMax returns minimum value in the map
func findMax(floats map[int32]int64) int64 {
	max := int64(math.MinInt64)
	for i := range floats {
		if floats[i] > max {
			max = floats[i]
		}
	}
	return max

}
