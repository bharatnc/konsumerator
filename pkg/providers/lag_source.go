package providers

import "time"

type LagSource interface {
	GetLag() map[int32]int64
	GetLagByPartition(int32) time.Duration
	QueryConsumptionRate() (map[int32]int64, error)
	QueryProductionRate() (map[int32]int64, error)
	QueryProductionRateDistribution() (map[int32]int64, error)
	QueryOffset() (map[int32]int64, error)
	EstimateLag() error
}
