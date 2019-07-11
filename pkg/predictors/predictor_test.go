package predictors

import (
	"github.com/google/go-cmp/cmp"
	"testing"

	autoscalev1 "github.com/kubernetes/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var (
	distr1 = map[int32]int64{
		2: 48898,
		3: 48239,
		5: 40290,
		4: 33853,
		0: 33327,
		6: 31712,
		9: 27088,
		1: 22428,
		8: 22339,
		7: 21661,
	}
	defaultPolicy = autoscalev1.ContainerResourcePolicy{
		ContainerName: "test",
		Mode:          nil,
		MinAllowed: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
		MaxAllowed: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("900m"),
			corev1.ResourceMemory: resource.MustParse("900Mi"),
		},
	}
)

func createSameResourceReqs(cpu string, memory string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		},
	}
}

func TestAssignResourcesBasedOnProductionRate(t *testing.T) {
	tests := map[string]struct {
		InPolicy       autoscalev1.ContainerResourcePolicy
		InDistribution map[int32]int64
		ExpReqs        map[int32]corev1.ResourceRequirements
	}{
		"expect to give calculate distribution": {
			InPolicy:       defaultPolicy,
			InDistribution: distr1,
			ExpReqs: map[int32]corev1.ResourceRequirements{
				0: *createSameResourceReqs("600m", "600Mi"),
				1: *createSameResourceReqs("200m", "200Mi"),
				2: *createSameResourceReqs("1000m", "1000Mi"),
				3: *createSameResourceReqs("1000m", "1000Mi"),
				4: *createSameResourceReqs("600m", "600Mi"),
				5: *createSameResourceReqs("800m", "800Mi"),
				6: *createSameResourceReqs("500m", "500Mi"),
				7: *createSameResourceReqs("200m", "200Mi"),
				8: *createSameResourceReqs("200m", "200Mi"),
				9: *createSameResourceReqs("300m", "300Mi"),
			},
		},
	}
	for testName, tt := range tests {
		reqs := AssignResourcesBasedOnProductionRate(tt.InPolicy, tt.InDistribution)
		if len(reqs) != len(tt.ExpReqs) {
			t.Fatalf("%s: expected to get %d reqs, got %d", testName, len(tt.ExpReqs), len(reqs))
		}
		for i := range reqs {
			actualRequests := tt.ExpReqs[i].Requests
			expectedRequests := reqs[i].Requests
			if expectedRequests.Cpu().MilliValue() != actualRequests.Cpu().MilliValue() {
				t.Fatalf("%s: bucket[%d]. expected request cpu %s, got %s", testName, i, actualRequests.Cpu().String(), expectedRequests.Cpu().String())
			}
			if actualRequests.Memory().MilliValue() != expectedRequests.Memory().MilliValue() {
				t.Fatalf("%s: bucket[%d]. expected request memory %s, got %s", testName, i, actualRequests.Memory().String(), expectedRequests.Memory().String())
			}
		}
	}
}

func TestMakeBuckets(t *testing.T) {
	tests := map[string]struct {
		MinP int64
		MaxP int64
		Size int64
		Exp  []int64
	}{
		"should not exceed max value with min step=1": {
			40,
			50,
			20,
			[]int64{40, 41, 42, 43, 44, 45, 46, 47, 48, 49},
		},
		"should produce buckets of 10": {
			40,
			50,
			10,
			[]int64{40, 41, 42, 43, 44, 45, 46, 47, 48, 49},
		},
		"should produce buckets of 5": {
			40,
			50,
			5,
			[]int64{40, 42, 44, 46, 48},
		},
		"should not fail if bucket size is 0": {
			40,
			50,
			0,
			nil,
		},
	}
	for testName, tt := range tests {
		buckets := makeBuckets(tt.MinP, tt.MaxP, tt.Size)
		if !cmp.Equal(buckets, tt.Exp) {
			t.Fatalf("%s: expected to get %v, but got %v", testName, tt.Exp, buckets)
		}
	}
}

func TestMakeResourceBuckets(t *testing.T) {
	tests := map[string]struct {
		minRes corev1.ResourceList
		maxRes corev1.ResourceList
		size   int64
		expRes []corev1.ResourceList
	}{
		"should split in 10 parts": {
			minRes: defaultPolicy.MinAllowed,
			maxRes: defaultPolicy.MaxAllowed,
			size:   10,
			expRes: []corev1.ResourceList{
				{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("300m"),
					corev1.ResourceMemory: resource.MustParse("300Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("400m"),
					corev1.ResourceMemory: resource.MustParse("400Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("500Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("600m"),
					corev1.ResourceMemory: resource.MustParse("600Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("700m"),
					corev1.ResourceMemory: resource.MustParse("700Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("800m"),
					corev1.ResourceMemory: resource.MustParse("800Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("900m"),
					corev1.ResourceMemory: resource.MustParse("900Mi"),
				},
				{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1000Mi"),
				},
			},
		},
	}
	for testName, tt := range tests {
		buckets := makeResourceBuckets(tt.minRes, tt.maxRes, tt.size)
		if len(buckets) != len(tt.expRes) {
			t.Fatalf("%s: expected to get %d buckets, got %d", testName, len(tt.expRes), len(buckets))
		}
		for i := range buckets {
			if buckets[i].Cpu().MilliValue() != tt.expRes[i].Cpu().MilliValue() {
				t.Fatalf("%s: bucket[%d]. expected cpu %s, got %s", testName, i, tt.expRes[i].Cpu().String(), buckets[i].Cpu().String())
			}
			if buckets[i].Memory().MilliValue() != tt.expRes[i].Memory().MilliValue() {
				t.Fatalf("%s: bucket[%d]. expected memory %s, got %s", testName, i, tt.expRes[i].Memory().String(), buckets[i].Memory().String())
			}
		}
	}
}

func TestGetBucketNum(t *testing.T) {
	buckets := []int64{30, 40, 50, 60, 70, 80}
	tests := map[string]struct {
		num     int64
		buckets []int64
		expRes  int64
		expErr  bool
	}{
		"empty buckets list": {
			20,
			[]int64{},
			0,
			true,
		},
		"less than lower bound": {
			20,
			buckets,
			0,
			false,
		},
		"lower bound": {
			30,
			buckets,
			1,
			false,
		},
		"high bound": {
			80,
			buckets,
			5,
			false,
		},
		"highier than high bound": {
			81,
			buckets,
			5,
			false,
		},
		"middle": {
			45,
			buckets,
			2,
			false,
		},
	}
	for testName, tt := range tests {
		b, err := getBucketNum(tt.num, tt.buckets)
		if err != nil && !tt.expErr {
			t.Fatalf("%s: unexpected error %v", testName, err)
		}
		if err == nil && tt.expErr {
			t.Fatalf("%s: expected error didn't happen", testName)
		}
		if b != tt.expRes {
			t.Fatalf("%s: expected to get %d, got %d", testName, tt.expRes, b)
		}
	}

}
