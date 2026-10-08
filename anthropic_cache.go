package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	HTTPCode  int       `json:"http_code,omitempty"`
	Expired   bool      `json:"expired,omitempty"`
	Failed    bool      `json:"failed,omitempty"`
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

func writeAnthropicCache(token string, cache anthropicCache) error {
	if anthropicCacheDir == "" {
		return errors.New("Anthropic cache directory unavailable")
	}
	if err := os.MkdirAll(anthropicCacheDir, 0o700); err != nil {
		return err
	}
	return writeAnthropicState(anthropicCachePath(token), cache)
}

func writeAnthropicState(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "anthropic-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}

const anthropicRequestInterval = 15 * time.Minute

type anthropicRequestState struct {
	NextAt      time.Time `json:"next_at"`
	RateLimited bool      `json:"rate_limited,omitempty"`
}

// A machine-wide gate survives token rotation and process restarts. Its lock
// serializes reservation, API requests and Retry-After updates across processes.
func reserveAnthropicRequest(cache anthropicCache) (anthropicRequestState, func(), error) {
	var state anthropicRequestState
	if anthropicCacheDir == "" {
		return state, nil, errors.New("Anthropic cache unavailable; request skipped")
	}
	if err := os.MkdirAll(anthropicCacheDir, 0700); err != nil {
		return state, nil, errors.New("cannot persist Anthropic cooldown; request skipped")
	}
	lock := filepath.Join(anthropicCacheDir, "anthropic-request.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		// A crashed process can leave its lock behind. HTTP requests time out in
		// 15 seconds, so only recover locks older than two minutes.
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			_ = os.Remove(lock)
			err = os.Mkdir(lock, 0700)
		}
		if err != nil {
			return state, nil, errors.New("Anthropic request already in progress; retry shortly")
		}
	}
	release := func() { _ = os.Remove(lock) }
	path := filepath.Join(anthropicCacheDir, "anthropic-request.json")
	b, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(b, &state) != nil {
			release()
			return state, nil, errors.New("invalid Anthropic cooldown file; request skipped")
		}
	} else if !os.IsNotExist(err) {
		release()
		return state, nil, errors.New("cannot read Anthropic cooldown; request skipped")
	}
	// Migrate existing per-token caches without issuing an extra request.
	if next := cache.FetchedAt.Add(anthropicRequestInterval); !cache.FetchedAt.IsZero() && next.After(state.NextAt) {
		state.NextAt = next
	}
	if cache.RetryAt.After(state.NextAt) {
		state.NextAt, state.RateLimited = cache.RetryAt, true
	}
	if time.Now().Before(state.NextAt) {
		if err := writeAnthropicState(path, state); err != nil {
			release()
			return state, nil, errors.New("cannot persist Anthropic cooldown; request skipped")
		}
		release()
		return state, nil, nil
	}
	state = anthropicRequestState{NextAt: time.Now().Add(anthropicRequestInterval)}
	// Reserve before sending, so crashes and network failures also respect the limit.
	if err := writeAnthropicState(path, state); err != nil {
		release()
		return state, nil, errors.New("cannot persist Anthropic cooldown; request skipped")
	}
	return state, release, nil
}

func anthropicRetryAt(header string) time.Time {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(header); err == nil && at.After(time.Now()) {
		return at
	}
	return time.Now().Add(anthropicRequestInterval)
}

func cachedAnthropic(cache anthropicCache) ([]Gauge, string, error) {
	return cooldownAnthropic(cache, anthropicRequestState{NextAt: cache.RetryAt, RateLimited: true})
}

func cooldownAnthropic(cache anthropicCache, state anthropicRequestState) ([]Gauge, string, error) {
	// Keep expired-token errors recognizable to the interactive renewal prompt.
	if cache.HTTPCode == http.StatusUnauthorized {
		return nil, "", &httpStatusError{code: cache.HTTPCode, expired: cache.Expired}
	}
	wait := time.Until(state.NextAt).Round(time.Minute)
	if wait < time.Minute {
		wait = time.Minute
	}
	if len(cache.Items) > 0 && time.Since(cache.FetchedAt) < 2*time.Hour {
		age := time.Since(cache.FetchedAt).Round(time.Minute)
		return cache.Items, fmt.Sprintf("cached %s ago; retry in %s", formatDuration(age), formatDuration(wait)), nil
	}
	reason := "Anthropic request cooldown"
	if state.RateLimited {
		reason = "Anthropic rate limited"
	} else if cache.Failed {
		reason = "Anthropic request failed"
	}
	return nil, "", fmt.Errorf("%s; retry in %s", reason, formatDuration(wait))
}
