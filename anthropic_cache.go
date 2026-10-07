package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The cache is shared by TUI refreshes and separate aimeter processes.
// Its filename identifies the credential without storing the bearer token.
var anthropicCacheDir = defaultAnthropicCacheDir()

type anthropicCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	RetryAt   time.Time `json:"retry_at"`
	Items     []Gauge   `json:"items"`
}

func defaultAnthropicCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "aimeter")
}

func anthropicCachePath(token string) string {
	sum := sha256.Sum256([]byte(token))
	return filepath.Join(anthropicCacheDir, "anthropic-"+hex.EncodeToString(sum[:])+".json")
}

func readAnthropicCache(token string) anthropicCache {
	if anthropicCacheDir == "" {
		return anthropicCache{}
	}
	b, err := os.ReadFile(anthropicCachePath(token))
	if err != nil {
		return anthropicCache{}
	}
	var cache anthropicCache
	if json.Unmarshal(b, &cache) != nil {
		return anthropicCache{}
	}
	return cache
}

func writeAnthropicCache(token string, cache anthropicCache) {
	if anthropicCacheDir == "" || os.MkdirAll(anthropicCacheDir, 0o700) != nil {
		return
	}
	b, err := json.Marshal(cache)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(anthropicCacheDir, "anthropic-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if f.Chmod(0o600) != nil {
		f.Close()
		return
	}
	_, err = f.Write(b)
	if err != nil {
		f.Close()
		return
	}
	if f.Close() == nil {
		_ = os.Rename(f.Name(), anthropicCachePath(token))
	}
}

func anthropicRetryAt(header string) time.Time {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(header); err == nil && at.After(time.Now()) {
		return at
	}
	return time.Now().Add(5 * time.Minute)
}

func cachedAnthropic(cache anthropicCache) ([]Gauge, string, error) {
	wait := time.Until(cache.RetryAt).Round(time.Minute)
	if wait < time.Minute {
		wait = time.Minute
	}
	if len(cache.Items) > 0 && time.Since(cache.FetchedAt) < 2*time.Hour {
		age := time.Since(cache.FetchedAt).Round(time.Minute)
		note := fmt.Sprintf("cached %s ago; retry in %s", formatDuration(age), formatDuration(wait))
		return cache.Items, note, nil
	}
	return nil, "", fmt.Errorf("Anthropic rate limited; retry in %s", formatDuration(wait))
}
