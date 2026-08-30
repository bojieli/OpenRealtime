package migration

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
)

const bootstrapMethod = "paired_case_cluster_percentile_bootstrap_v2"

type binaryObservation struct {
	cluster             string
	baseline, candidate bool
}

type latencyObservation struct {
	cluster             string
	baseline, candidate float64
}

func distribution(values []float64, unit string) Distribution {
	result := Distribution{Count: len(values), Unit: unit}
	if len(values) == 0 {
		return result
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	result.Min = sorted[0]
	result.P50 = percentile(sorted, 0.50)
	result.P90 = percentile(sorted, 0.90)
	result.P95 = percentile(sorted, 0.95)
	result.P99 = percentile(sorted, 0.99)
	result.Max = sorted[len(sorted)-1]
	result.Mean = arithmeticMean(sorted)
	return result
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	position := fraction * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func statistic(values []float64, name LatencyStatistic) float64 {
	if len(values) == 0 {
		return 0
	}
	if name == StatisticMean {
		return arithmeticMean(values)
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	switch name {
	case StatisticP50:
		return percentile(sorted, 0.50)
	case StatisticP90:
		return percentile(sorted, 0.90)
	case StatisticP95:
		return percentile(sorted, 0.95)
	case StatisticP99:
		return percentile(sorted, 0.99)
	default:
		return 0
	}
}

// arithmeticMean avoids overflowing merely because several individually
// finite readings are close to MaxFloat64. The opposite-sign branch also
// avoids forming value-mean when that subtraction would overflow.
func arithmeticMean(values []float64) float64 {
	mean := 0.0
	count := int64(0)
	for _, value := range values {
		mean = combineMean(mean, count, value, 1)
		count++
	}
	return mean
}

func combineMean(mean float64, count int64, value float64, weight int64) float64 {
	if weight <= 0 {
		return mean
	}
	total := count + weight
	ratio := float64(weight) / float64(total)
	delta := value - mean
	if finite(delta) {
		return mean + delta*ratio
	}
	return mean*(float64(count)/float64(total)) + value*ratio
}

func binaryLowerBound(
	observations []binaryObservation, policy BootstrapPolicy, label string,
) ConfidenceBound {
	clusters := groupBinary(observations)
	seed := derivedSeed(policy.Seed, label)
	random := splitMix64(seed)
	readings := make([]float64, policy.Resamples)
	for sample := range readings {
		difference, count := 0.0, 0
		for range clusters {
			cluster := clusters[random.index(len(clusters))]
			difference += cluster.difference
			count += cluster.count
		}
		readings[sample] = difference / float64(count)
	}
	sort.Float64s(readings)
	return ConfidenceBound{
		Level: policy.Confidence, Direction: "lower",
		Value:  conservativeLowerQuantile(readings, 1-policy.Confidence),
		Method: bootstrapMethod, Resamples: policy.Resamples, Seed: seed,
	}
}

func latencyUpperBounds(
	observations []latencyObservation, statisticNames []LatencyStatistic,
	policy BootstrapPolicy, label string,
) map[LatencyStatistic]ConfidenceBound {
	clusters := groupLatency(observations)
	seed := derivedSeed(policy.Seed, label)
	random := splitMix64(seed)
	statistics := uniqueStatistics(statisticNames)
	readings := make(map[LatencyStatistic][]float64, len(statistics))
	for _, name := range statistics {
		readings[name] = make([]float64, policy.Resamples)
	}
	baselineOrder, candidateOrder := orderedLatencyValues(clusters)
	weights := make([]int64, len(clusters))
	for sample := 0; sample < policy.Resamples; sample++ {
		clear(weights)
		for range clusters {
			weights[random.index(len(clusters))]++
		}
		total := weightedObservationCount(clusters, weights)
		baseline := weightedStatistics(baselineOrder, weights, total, statistics)
		candidate := weightedStatistics(candidateOrder, weights, total, statistics)
		for _, name := range statistics {
			readings[name][sample] = candidate[name] - baseline[name]
		}
	}
	result := make(map[LatencyStatistic]ConfidenceBound, len(statistics))
	for _, name := range statistics {
		values := readings[name]
		sort.Float64s(values)
		result[name] = ConfidenceBound{
			Level: policy.Confidence, Direction: "upper",
			Value:  conservativeUpperQuantile(values, policy.Confidence),
			Method: bootstrapMethod, Resamples: policy.Resamples, Seed: seed,
		}
	}
	return result
}

func uniqueStatistics(input []LatencyStatistic) []LatencyStatistic {
	seen := make(map[LatencyStatistic]bool, len(input))
	result := make([]LatencyStatistic, 0, len(input))
	for _, name := range input {
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

type weightedLatencyValue struct {
	value   float64
	cluster int
}

func orderedLatencyValues(clusters []latencyCluster) ([]weightedLatencyValue, []weightedLatencyValue) {
	count := 0
	for _, cluster := range clusters {
		count += len(cluster.baseline)
	}
	baseline := make([]weightedLatencyValue, 0, count)
	candidate := make([]weightedLatencyValue, 0, count)
	for clusterIndex, cluster := range clusters {
		for _, value := range cluster.baseline {
			baseline = append(baseline, weightedLatencyValue{value: value, cluster: clusterIndex})
		}
		for _, value := range cluster.candidate {
			candidate = append(candidate, weightedLatencyValue{value: value, cluster: clusterIndex})
		}
	}
	less := func(values []weightedLatencyValue) func(int, int) bool {
		return func(left, right int) bool {
			if values[left].value != values[right].value {
				return values[left].value < values[right].value
			}
			return values[left].cluster < values[right].cluster
		}
	}
	sort.Slice(baseline, less(baseline))
	sort.Slice(candidate, less(candidate))
	return baseline, candidate
}

func weightedObservationCount(clusters []latencyCluster, weights []int64) int64 {
	total := int64(0)
	for index, cluster := range clusters {
		total += int64(len(cluster.baseline)) * weights[index]
	}
	return total
}

type weightedQuantile struct {
	name                   LatencyStatistic
	fraction               float64
	lowerRank, upperRank   int64
	lowerValue, upperValue float64
	lowerSet, upperSet     bool
}

func weightedStatistics(
	ordered []weightedLatencyValue, weights []int64, total int64,
	statistics []LatencyStatistic,
) map[LatencyStatistic]float64 {
	result := make(map[LatencyStatistic]float64, len(statistics))
	quantiles := make([]weightedQuantile, 0, len(statistics))
	needMean := false
	for _, name := range statistics {
		if name == StatisticMean {
			needMean = true
			continue
		}
		fraction := statisticFraction(name)
		position := fraction * float64(total-1)
		quantiles = append(quantiles, weightedQuantile{
			name: name, fraction: fraction,
			lowerRank: int64(math.Floor(position)), upperRank: int64(math.Ceil(position)),
		})
	}
	mean, meanCount, rank := 0.0, int64(0), int64(0)
	for _, reading := range ordered {
		weight := weights[reading.cluster]
		if weight == 0 {
			continue
		}
		if needMean {
			mean = combineMean(mean, meanCount, reading.value, weight)
			meanCount += weight
		}
		nextRank := rank + weight
		for index := range quantiles {
			target := &quantiles[index]
			if !target.lowerSet && target.lowerRank >= rank && target.lowerRank < nextRank {
				target.lowerValue, target.lowerSet = reading.value, true
			}
			if !target.upperSet && target.upperRank >= rank && target.upperRank < nextRank {
				target.upperValue, target.upperSet = reading.value, true
			}
		}
		rank = nextRank
	}
	if needMean {
		result[StatisticMean] = mean
	}
	for _, target := range quantiles {
		position := target.fraction * float64(total-1)
		weight := position - float64(target.lowerRank)
		result[target.name] = target.lowerValue*(1-weight) + target.upperValue*weight
	}
	return result
}

func statisticFraction(name LatencyStatistic) float64 {
	switch name {
	case StatisticP50:
		return 0.50
	case StatisticP90:
		return 0.90
	case StatisticP95:
		return 0.95
	case StatisticP99:
		return 0.99
	default:
		return 0
	}
}

func conservativeLowerQuantile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Floor(fraction*float64(len(sorted)+1))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func conservativeUpperQuantile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(fraction*float64(len(sorted)+1))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

type binaryCluster struct {
	identity   string
	difference float64
	count      int
}

func groupBinary(observations []binaryObservation) []binaryCluster {
	byIdentity := make(map[string]*binaryCluster)
	for _, observation := range observations {
		cluster := byIdentity[observation.cluster]
		if cluster == nil {
			cluster = &binaryCluster{identity: observation.cluster}
			byIdentity[observation.cluster] = cluster
		}
		cluster.difference += truth(observation.candidate) - truth(observation.baseline)
		cluster.count++
	}
	result := make([]binaryCluster, 0, len(byIdentity))
	for _, cluster := range byIdentity {
		result = append(result, *cluster)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].identity < result[right].identity
	})
	return result
}

type latencyCluster struct {
	identity            string
	baseline, candidate []float64
}

func groupLatency(observations []latencyObservation) []latencyCluster {
	byIdentity := make(map[string]*latencyCluster)
	for _, observation := range observations {
		cluster := byIdentity[observation.cluster]
		if cluster == nil {
			cluster = &latencyCluster{identity: observation.cluster}
			byIdentity[observation.cluster] = cluster
		}
		cluster.baseline = append(cluster.baseline, observation.baseline)
		cluster.candidate = append(cluster.candidate, observation.candidate)
	}
	result := make([]latencyCluster, 0, len(byIdentity))
	for _, cluster := range byIdentity {
		result = append(result, *cluster)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].identity < result[right].identity
	})
	return result
}

func derivedSeed(seed uint64, label string) uint64 {
	input := make([]byte, 8+len(label))
	binary.LittleEndian.PutUint64(input, seed)
	copy(input[8:], label)
	digest := sha256.Sum256(input)
	value := binary.LittleEndian.Uint64(digest[:8])
	if value == 0 {
		return 1
	}
	return value
}

// splitMix64 is embedded rather than delegated to math/rand so an archived
// manifest keeps the same inference stream across Go library revisions.
type splitMix64 uint64

func (random *splitMix64) next() uint64 {
	*random += 0x9e3779b97f4a7c15
	value := uint64(*random)
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func (random *splitMix64) index(size int) int {
	if size <= 1 {
		return 0
	}
	bound := uint64(size)
	threshold := -bound % bound
	for {
		value := random.next()
		if value >= threshold {
			return int(value % bound)
		}
	}
}

func truth(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
