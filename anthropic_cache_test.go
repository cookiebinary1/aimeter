package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicRateLimitUsesCacheAndCooldown(t *testing.T) {
	oldDir, oldTransport := anthropicCacheDir, httpc.Transport
	anthropicCacheDir = t.TempDir()
	t.Cleanup(func() {
		anthropicCacheDir = oldDir
		httpc.Transport = oldTransport
	})
	const token = "test-token"
	writeAnthropicCache(token, anthropicCache{
		FetchedAt: time.Now().Add(-10 * time.Minute),
		Items:     []Gauge{{Label: "5h", Used: 42}},
	})
	calls := 0
	httpc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"120"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
		}, nil
	})
	for i := 0; i < 2; i++ {
		gs, note, err := fetchAnthropic(context.Background(), Creds{AnthToken: token})
		if err != nil || len(gs) != 1 || gs[0].Used != 42 || !strings.Contains(note, "cached") {
			t.Fatalf("fetch %d: gauges=%v note=%q err=%v", i, gs, note, err)
		}
	}
	if calls != 1 {
		t.Fatalf("HTTP calls=%d, want 1", calls)
	}
	if !readAnthropicCache(token).RetryAt.After(time.Now().Add(time.Minute)) {
		t.Fatal("retry time was not persisted")
	}
}

func TestAnthropicRateLimitWithoutCacheShowsWait(t *testing.T) {
	_, _, err := cachedAnthropic(anthropicCache{RetryAt: time.Now().Add(30 * time.Minute)})
	if err == nil || !strings.Contains(err.Error(), "retry in 30m") {
		t.Fatalf("error = %v", err)
	}
}
