package helpers

import (
	"strconv"

	v12 "k8s.io/api/core/v1"
)

func Ptr2Int32(i int32) *int32 {
	return &i
}

func Ptr2Int64(i int64) *int64 {
	return &i
}

func ParsePartitionAnnotation(partition string) *int32 {
	if len(partition) == 0 {
		return nil
	}
	p, err := strconv.ParseInt(partition, 10, 32)
	if err != nil {
		return nil
	}
	p32 := int32(p)
	return &p32
}

func SetEnvVariable(envVars []v12.EnvVar, key string, value string) []v12.EnvVar {
	for i, e := range envVars {
		if e.Name == key {
			envVars[i].Value = value
			return envVars
		}
	}
	envVars = append(envVars, v12.EnvVar{
		Name:  key,
		Value: value,
	})
	return envVars
}
