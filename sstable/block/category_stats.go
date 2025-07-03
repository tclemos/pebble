// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package block

import (
	"cmp"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
	"github.com/cockroachdb/pebble/sstable/block/blockkind"
)

// Category is a user-understandable string, where stats are aggregated for
// each category. The cardinality of this should be low, say < 20. The prefix
// "pebble-" is reserved for internal Pebble categories.
//
// Examples of categories that can be useful in the CockroachDB context are:
// sql-user, sql-stats, raft, rangefeed, mvcc-gc, range-snapshot.
type Category uint8

// CategoryUnknown is the unknown category. It has the latency-sensitive QoS
// level.
const CategoryUnknown Category = 0

// CategoryMax is the maximum value of a category, and is also the maximum
// number of categories that can be registered.
const CategoryMax = 30

// shardPadding pads each shard to a cache line boundary so they don't share a cache line.
// The CategoryStatsShard struct is now larger due to detailed metrics, so we use 512 bytes.
const shardPadding = 512 - unsafe.Sizeof(CategoryStatsShard{})

// paddedCategoryStatsShard is a single shard of a category's statistics.
type paddedCategoryStatsShard struct {
	CategoryStatsShard
	_ [shardPadding]byte
}

func (c Category) String() string {
	return categories[c].name
}

// QoSLevel returns the QoSLevel associated with this Category.
func (c Category) QoSLevel() QoSLevel {
	return categories[c].qosLevel
}

// SafeFormat implements the redact.SafeFormatter interface.
func (c Category) SafeFormat(p redact.SafePrinter, verb rune) {
	p.SafeString(redact.SafeString(c.String()))
}

// RegisterCategory registers a new category. Each category has a name and an
// associated QoS level. The category name must be unique.
//
// Only CategoryMax categories can be registered in total.
func RegisterCategory(name string, qosLevel QoSLevel) Category {
	if categoriesList != nil {
		panic("ReigsterCategory called after Categories()")
	}
	c := Category(numRegisteredCategories.Add(1))
	if c > CategoryMax {
		panic("too many categories")
	}
	categories[c].name = name
	categories[c].qosLevel = qosLevel
	return c
}

// Categories returns all registered categories, including CategoryUnknown.
//
// Can only be called after all categories have been registered. Calling
// RegisterCategory() after Categories() will result in a panic.
func Categories() []Category {
	categoriesListOnce.Do(func() {
		categoriesList = make([]Category, numRegisteredCategories.Load()+1)
		for i := range categoriesList {
			categoriesList[i] = Category(i)
		}
	})
	return categoriesList
}

var categories = [CategoryMax + 1]struct {
	name     string
	qosLevel QoSLevel
}{
	CategoryUnknown: {name: "unknown", qosLevel: LatencySensitiveQoSLevel},
}

var numRegisteredCategories atomic.Uint32

var categoriesList []Category
var categoriesListOnce sync.Once

// StringToCategoryForTesting returns the Category for the string, or panics if
// the string is not known.
func StringToCategoryForTesting(s string) Category {
	for i := range categories {
		if categories[i].name == s {
			return Category(i)
		}
	}
	panic(errors.AssertionFailedf("unknown Category %s", s))
}

// QoSLevel describes whether the read is latency-sensitive or not. Each
// category must map to a single QoSLevel. While category strings are opaque
// to Pebble, the QoSLevel may be internally utilized in Pebble to better
// optimize future reads.
type QoSLevel uint8

const (
	// LatencySensitiveQoSLevel is the default when QoSLevel is not specified,
	// and represents reads that are latency-sensitive.
	LatencySensitiveQoSLevel QoSLevel = iota
	// NonLatencySensitiveQoSLevel represents reads that are not
	// latency-sensitive.
	NonLatencySensitiveQoSLevel
)

// SafeFormat implements the redact.SafeFormatter interface.
func (q QoSLevel) SafeFormat(p redact.SafePrinter, verb rune) {
	switch q {
	case LatencySensitiveQoSLevel:
		p.Printf("latency")
	case NonLatencySensitiveQoSLevel:
		p.Printf("non-latency")
	default:
		p.Printf("<unknown-qos>")
	}
}

// StringToQoSForTesting returns the QoSLevel for the string, or panics if the
// string is not known.
func StringToQoSForTesting(s string) QoSLevel {
	switch s {
	case "latency":
		return LatencySensitiveQoSLevel
	case "non-latency":
		return NonLatencySensitiveQoSLevel
	}
	panic(errors.AssertionFailedf("unknown QoS %s", s))
}

// CategoryStats provides stats about a category of reads.
type CategoryStats struct {
	// BlockBytes is the bytes in the loaded blocks. If the block was
	// compressed, this is the compressed bytes. Currently, only the index
	// blocks, data blocks containing points, and filter blocks are included.
	// Additionally, value blocks read after the corresponding iterator is
	// closed are not included.
	BlockBytes uint64
	// BlockBytesInCache is the subset of BlockBytes that were in the block
	// cache.
	BlockBytesInCache uint64
	// BlockReadDuration is the total duration to read the bytes not in the
	// cache, i.e., BlockBytes-BlockBytesInCache.
	BlockReadDuration time.Duration
	
	// High-resolution instrumentation for cache performance analysis
	
	// CacheHitsByBlockKind tracks cache hits by block type
	CacheHitsByBlockKind [blockkind.NumKinds]uint64
	// CacheMissesByBlockKind tracks cache misses by block type  
	CacheMissesByBlockKind [blockkind.NumKinds]uint64
	// CacheHitsByLevel tracks cache hits by LSM level (0-6)
	CacheHitsByLevel [7]uint64
	// CacheMissesByLevel tracks cache misses by LSM level (0-6)
	CacheMissesByLevel [7]uint64
	// ReadLatencyByLevel tracks cumulative read time per LSM level
	ReadLatencyByLevel [7]time.Duration
	// BlocksReadByLevel tracks number of blocks read per LSM level
	BlocksReadByLevel [7]uint64
}

func (s *CategoryStats) aggregate(
	blockBytes, blockBytesInCache uint64, blockReadDuration time.Duration,
) {
	s.BlockBytes += blockBytes
	s.BlockBytesInCache += blockBytesInCache
	s.BlockReadDuration += blockReadDuration
}

// aggregateDetailed adds detailed instrumentation metrics to the stats.
func (s *CategoryStats) aggregateDetailed(
	blockBytes, blockBytesInCache uint64, blockReadDuration time.Duration,
	blockKind blockkind.Kind, level int, cacheHit bool,
) {
	// Aggregate basic stats
	s.aggregate(blockBytes, blockBytesInCache, blockReadDuration)
	
	// Track cache hits/misses by block kind
	if int(blockKind) < len(s.CacheHitsByBlockKind) {
		if cacheHit {
			s.CacheHitsByBlockKind[blockKind]++
		} else {
			s.CacheMissesByBlockKind[blockKind]++
		}
	}
	
	// Track cache hits/misses by level (0-6)
	if level >= 0 && level < len(s.CacheHitsByLevel) {
		if cacheHit {
			s.CacheHitsByLevel[level]++
		} else {
			s.CacheMissesByLevel[level]++
		}
		s.ReadLatencyByLevel[level] += blockReadDuration
		s.BlocksReadByLevel[level]++
	}
}

// aggregateFrom combines stats from another CategoryStats instance.
func (s *CategoryStats) aggregateFrom(other *CategoryStats) {
	s.BlockBytes += other.BlockBytes
	s.BlockBytesInCache += other.BlockBytesInCache
	s.BlockReadDuration += other.BlockReadDuration
	
	// Aggregate detailed metrics
	for i := range s.CacheHitsByBlockKind {
		s.CacheHitsByBlockKind[i] += other.CacheHitsByBlockKind[i]
		s.CacheMissesByBlockKind[i] += other.CacheMissesByBlockKind[i]
	}
	
	for i := range s.CacheHitsByLevel {
		s.CacheHitsByLevel[i] += other.CacheHitsByLevel[i]
		s.CacheMissesByLevel[i] += other.CacheMissesByLevel[i]
		s.ReadLatencyByLevel[i] += other.ReadLatencyByLevel[i]
		s.BlocksReadByLevel[i] += other.BlocksReadByLevel[i]
	}
}

// Helper methods for analyzing detailed metrics

// CacheHitRate returns the overall cache hit rate as a percentage.
func (s *CategoryStats) CacheHitRate() float64 {
	totalHits := uint64(0)
	totalMisses := uint64(0)
	
	for i := range s.CacheHitsByBlockKind {
		totalHits += s.CacheHitsByBlockKind[i]
		totalMisses += s.CacheMissesByBlockKind[i]
	}
	
	total := totalHits + totalMisses
	if total == 0 {
		return 0.0
	}
	return float64(totalHits) / float64(total) * 100.0
}

// CacheHitRateByBlockKind returns the cache hit rate for a specific block kind.
func (s *CategoryStats) CacheHitRateByBlockKind(kind blockkind.Kind) float64 {
	if int(kind) >= len(s.CacheHitsByBlockKind) {
		return 0.0
	}
	
	hits := s.CacheHitsByBlockKind[kind]
	misses := s.CacheMissesByBlockKind[kind]
	total := hits + misses
	
	if total == 0 {
		return 0.0
	}
	return float64(hits) / float64(total) * 100.0
}

// CacheHitRateByLevel returns the cache hit rate for a specific LSM level.
func (s *CategoryStats) CacheHitRateByLevel(level int) float64 {
	if level < 0 || level >= len(s.CacheHitsByLevel) {
		return 0.0
	}
	
	hits := s.CacheHitsByLevel[level]
	misses := s.CacheMissesByLevel[level]
	total := hits + misses
	
	if total == 0 {
		return 0.0
	}
	return float64(hits) / float64(total) * 100.0
}

// AverageReadLatencyByLevel returns the average read latency for a specific LSM level.
func (s *CategoryStats) AverageReadLatencyByLevel(level int) time.Duration {
	if level < 0 || level >= len(s.ReadLatencyByLevel) {
		return 0
	}
	
	blocks := s.BlocksReadByLevel[level]
	if blocks == 0 {
		return 0
	}
	
	return s.ReadLatencyByLevel[level] / time.Duration(blocks)
}

// TotalCacheHits returns the total number of cache hits across all block kinds and levels.
func (s *CategoryStats) TotalCacheHits() uint64 {
	total := uint64(0)
	for i := range s.CacheHitsByBlockKind {
		total += s.CacheHitsByBlockKind[i]
	}
	return total
}

// TotalCacheMisses returns the total number of cache misses across all block kinds and levels.
func (s *CategoryStats) TotalCacheMisses() uint64 {
	total := uint64(0)
	for i := range s.CacheMissesByBlockKind {
		total += s.CacheMissesByBlockKind[i]
	}
	return total
}

// CategoryStatsAggregate is the aggregate for the given category.
type CategoryStatsAggregate struct {
	Category      Category
	CategoryStats CategoryStats
}

// numCategoryStatsShards must be a power of 2. We initialize it to GOMAXPROCS
// (rounded up to the nearest power of 2) or 16, whichever is larger.
var numCategoryStatsShards = func() int {
	p := runtime.GOMAXPROCS(0)
	n := 16
	for n < p {
		n *= 2
	}
	return n
}()

// CategoryStatsShard holds CategoryStats with a mutex
// to ensure safe access.
type CategoryStatsShard struct {
	mu struct {
		sync.Mutex
		stats CategoryStats
	}
}

// Accumulate implements the IterStatsAccumulator interface.
func (c *CategoryStatsShard) Accumulate(
	blockBytes, blockBytesInCache uint64, blockReadDuration time.Duration,
) {
	c.mu.Lock()
	c.mu.stats.aggregate(blockBytes, blockBytesInCache, blockReadDuration)
	c.mu.Unlock()
}

// AccumulateDetailed accumulates detailed instrumentation metrics.
func (c *CategoryStatsShard) AccumulateDetailed(
	blockBytes, blockBytesInCache uint64, blockReadDuration time.Duration,
	blockKind blockkind.Kind, level int, cacheHit bool,
) {
	c.mu.Lock()
	c.mu.stats.aggregateDetailed(blockBytes, blockBytesInCache, blockReadDuration, blockKind, level, cacheHit)
	c.mu.Unlock()
}

// CategoryStatsCollector collects and aggregates the stats per category.
type CategoryStatsCollector struct {
	// mu protects additions to statsMap.
	mu sync.Mutex
	// Category => *shardedCategoryStats.
	statsMap sync.Map
}

// shardedCategoryStats accumulates stats for a category, splitting its stats
// across multiple shards to prevent mutex contention. In high-read workloads,
// contention on the category stats mutex has been observed.
type shardedCategoryStats struct {
	Category Category
	shards   []paddedCategoryStatsShard
}

// getStats retrieves the aggregated stats for the category, summing across all
// shards.
func (s *shardedCategoryStats) getStats() CategoryStatsAggregate {
	agg := CategoryStatsAggregate{
		Category: s.Category,
	}
	for i := range s.shards {
		s.shards[i].mu.Lock()
		agg.CategoryStats.aggregateFrom(&s.shards[i].mu.stats)
		s.shards[i].mu.Unlock()
	}
	return agg
}

// Accumulator returns a stats accumulator for the given category. The provided
// p is used to detrmine which shard to write stats to.
func (c *CategoryStatsCollector) Accumulator(p uint64, category Category) *CategoryStatsShard {
	v, ok := c.statsMap.Load(category)
	if !ok {
		c.mu.Lock()
		v, _ = c.statsMap.LoadOrStore(category, &shardedCategoryStats{
			Category: category,
			shards:   make([]paddedCategoryStatsShard, numCategoryStatsShards),
		})
		c.mu.Unlock()
	}
	s := v.(*shardedCategoryStats)
	// This equation is taken from:
	// https://en.wikipedia.org/wiki/Linear_congruential_generator#Parameters_in_common_use
	shard := ((p * 25214903917) >> 32) & uint64(numCategoryStatsShards-1)
	return &s.shards[shard].CategoryStatsShard
}

// GetStats returns the aggregated stats.
func (c *CategoryStatsCollector) GetStats() []CategoryStatsAggregate {
	var stats []CategoryStatsAggregate
	c.statsMap.Range(func(_, v any) bool {
		s := v.(*shardedCategoryStats).getStats()
		stats = append(stats, s)
		return true
	})
	slices.SortFunc(stats, func(a, b CategoryStatsAggregate) int {
		return cmp.Compare(a.Category, b.Category)
	})
	return stats
}
