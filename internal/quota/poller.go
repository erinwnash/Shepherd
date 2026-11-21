package quota

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
)

type pollCacheEntry struct {
	buckets   []Bucket
	timestamp time.Time
}

var (
	pollMu    sync.Mutex
	pollCache = make(map[string]pollCacheEntry)
)

// PollCommand executes a usage command and parses its output into quota buckets.
func PollCommand(ctx context.Context, usageCmd []string, parser string) ([]Bucket, error) {
	if len(usageCmd) == 0 {
		return nil, fmt.Errorf("usage_cmd is empty")
	}

	pollCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	cmd := exec.CommandContext(pollCtx, usageCmd[0], usageCmd[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("usage_cmd %v failed: %w (output: %s)", usageCmd, err, strings.TrimSpace(string(out)))
	}

	raw := string(out)
	switch parser {
	case "agy-usage":
		return ParseAgyUsage(raw)
	case "claude-usage":
		return ParseClaudeUsage(raw, time.Now())
	default:
		if strings.Contains(raw, "Gemini") || strings.Contains(raw, "Claude") {
			return ParseAgyUsage(raw)
		}
		return ParseClaudeUsage(raw, time.Now())
	}
}

// PollPoolCached executes or returns cached bucket readings for a given pool configuration.
// Since 'agy -p /usage' reports both Gemini and Claude buckets at once, the cache key is
// based on the usage command string, sharing cache hits across different pools.
func PollPoolCached(ctx context.Context, poolName string, pc config.PoolConfig, ttl time.Duration) ([]Bucket, error) {
	if pc.Mode != ModePolled || len(pc.UsageCmd) == 0 {
		return nil, nil
	}

	cacheKey := strings.Join(pc.UsageCmd, " ") + "::" + pc.Parser

	pollMu.Lock()
	entry, found := pollCache[cacheKey]
	if found && time.Since(entry.timestamp) < ttl {
		pollMu.Unlock()
		return filterBucketsForPool(entry.buckets, poolName), nil
	}
	pollMu.Unlock()

	buckets, err := PollCommand(ctx, pc.UsageCmd, pc.Parser)
	if err != nil {
		pollMu.Lock()
		if found {
			pollMu.Unlock()
			return filterBucketsForPool(entry.buckets, poolName), nil
		}
		pollMu.Unlock()
		return nil, err
	}

	pollMu.Lock()
	pollCache[cacheKey] = pollCacheEntry{
		buckets:   buckets,
		timestamp: time.Now(),
	}
	pollMu.Unlock()

	return filterBucketsForPool(buckets, poolName), nil
}

func filterBucketsForPool(buckets []Bucket, poolName string) []Bucket {
	var res []Bucket
	pLower := strings.ToLower(poolName)
	for _, b := range buckets {
		bLower := strings.ToLower(b.Pool)
		if bLower == pLower {
			res = append(res, b)
		} else if (pLower == "cc" || pLower == "claude") && (bLower == "cc" || bLower == "claude") {
			copyBucket := b
			copyBucket.Pool = poolName
			res = append(res, copyBucket)
		}
	}
	return res
}

// PollAllPools polls all polled pools concurrently or via cache and returns a map of pool name -> weekly Bucket.
func PollAllPools(ctx context.Context, pools map[string]config.PoolConfig) map[string]Bucket {
	readings := make(map[string]Bucket)
	for name, pc := range pools {
		if pc.Mode == ModePolled && len(pc.UsageCmd) > 0 {
			buckets, err := PollPoolCached(ctx, name, pc, 15*time.Second)
			if err == nil {
				for _, b := range buckets {
					if b.Window == "weekly" {
						readings[name] = b
						break
					}
					if _, ok := readings[name]; !ok {
						readings[name] = b
					}
				}
			}
		}
	}
	return readings
}
