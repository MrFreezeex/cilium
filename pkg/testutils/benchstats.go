// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package testutils

import (
	"fmt"
	"math"
	"runtime"

	"github.com/cilium/cilium/pkg/time"
)

// MemoryPair holds memory statistics before and after an operation
type MemoryPair struct {
	Before runtime.MemStats
	After  runtime.MemStats
}

// MemoryStats holds aggregated memory statistics
type MemoryStats struct {
	Objects, Alloc, InUse int64
}

// PrintMemoryStats prints formatted memory statistics from multiple runs
func PrintMemoryStats(pairs []*MemoryPair, testSize int) {
	min, max, avg := CalculateMemoryStatistics(pairs)
	fmt.Printf("Min: Allocated %6dkB in total, %7d objects / %6dkB still reachable (per service: %3d objs, %5dB alloc, %5dB in-use)\n", min.Alloc/1024, min.Objects, min.InUse/1024, min.Objects/int64(testSize), min.Alloc/int64(testSize), min.InUse/int64(testSize))
	fmt.Printf("Avg: Allocated %6dkB in total, %7d objects / %6dkB still reachable (per service: %3d objs, %5dB alloc, %5dB in-use)\n", avg.Alloc/1024, avg.Objects, avg.InUse/1024, avg.Objects/int64(testSize), avg.Alloc/int64(testSize), avg.InUse/int64(testSize))
	fmt.Printf("Max: Allocated %6dkB in total, %7d objects / %6dkB still reachable (per service: %3d objs, %5dB alloc, %5dB in-use)\n", max.Alloc/1024, max.Objects, max.InUse/1024, max.Objects/int64(testSize), max.Alloc/int64(testSize), max.InUse/int64(testSize))
}

// CalculateMemoryStatistics computes min, max, and average memory statistics
func CalculateMemoryStatistics(pairs []*MemoryPair) (Min, Max, Avg MemoryStats) {
	Min.Objects = math.MaxInt64
	Min.Alloc = math.MaxInt64
	Min.InUse = math.MaxInt64
	for _, memory := range pairs {
		var objects, alloc, inUse int64
		objects = int64(memory.After.HeapObjects - memory.Before.HeapObjects)
		Min.Objects = min(Min.Objects, objects)
		Max.Objects = max(Max.Objects, objects)
		Avg.Objects += objects

		alloc = int64(memory.After.TotalAlloc - memory.Before.TotalAlloc)
		Min.Alloc = min(Min.Alloc, alloc)
		Max.Alloc = max(Max.Alloc, alloc)
		Avg.Alloc += alloc

		inUse = int64(memory.After.HeapAlloc - memory.Before.HeapAlloc)
		Min.InUse = min(Min.InUse, inUse)
		Max.InUse = max(Max.InUse, inUse)
		Avg.InUse += inUse
	}
	Avg.Objects /= int64(len(pairs))
	Avg.Alloc /= int64(len(pairs))
	Avg.InUse /= int64(len(pairs))
	return
}

// PrintTimeStats prints formatted time statistics from multiple runs
func PrintTimeStats(durations []time.Duration, testSize int) {
	min, max, avg := CalculateTimeStats(durations)
	avgPerService := time.Duration(avg.Nanoseconds() / int64(testSize))
	maxPerService := time.Duration(max.Nanoseconds() / int64(testSize))
	minPerService := time.Duration(min.Nanoseconds() / int64(testSize))

	fmt.Printf("Min: Reconciled %d objects in %-11s (%-9s per service / %6.0f services per second)\n", testSize, min, minPerService, float64(time.Second)/float64(minPerService))
	fmt.Printf("Avg: Reconciled %d objects in %-11s (%-9s per service / %6.0f services per second)\n", testSize, avg, avgPerService, float64(time.Second)/float64(avgPerService))
	fmt.Printf("Max: Reconciled %d objects in %-11s (%-9s per service / %6.0f services per second)\n", testSize, max, maxPerService, float64(time.Second)/float64(maxPerService))
}

// CalculateTimeStats computes min, max, and average time statistics
func CalculateTimeStats(durations []time.Duration) (Min, Max, Avg time.Duration) {
	var sum time.Duration
	Min = 2 * time.Hour
	for _, duration := range durations {
		Min = min(Min, duration)
		Max = max(Max, duration)
		sum += duration
	}
	Avg = time.Duration(sum.Nanoseconds() / int64(len(durations)) * int64(time.Nanosecond))
	return
}

// MapFunc applies a function to each element of a slice
func MapFunc[A, B any](xs []A, fn func(A) B) []B {
	out := make([]B, len(xs))
	for i := range xs {
		out[i] = fn(xs[i])
	}
	return out
}
