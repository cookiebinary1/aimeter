package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		FetchedAt: time.Now().Add(-20 * time.Minute),
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

func isolateAnthropicCache(t *testing.T) {
	t.Helper()
	oldDir, oldTransport := anthropicCacheDir, httpc.Transport
	anthropicCacheDir = t.TempDir()
	t.Cleanup(func() { anthropicCacheDir, httpc.Transport = oldDir, oldTransport })
}
func TestAnthropicCooldownSurvivesRestartAndTokenRotation(t *testing.T) {
	isolateAnthropicCache(t)
	calls := 0
	httpc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":42}}`))}, nil
	})
	if _, _, err := fetchAnthropic(context.Background(), Creds{AnthToken: "first"}); err != nil {
		t.Fatal(err)
	}
	// Every call loads state from disk, as a new process would.
	gs, note, err := fetchAnthropic(context.Background(), Creds{AnthToken: "first"})
	if err != nil || len(gs) != 1 || !strings.Contains(note, "cached") {
		t.Fatalf("cached result: %v %q %v", gs, note, err)
	}
	_, _, err = fetchAnthropic(context.Background(), Creds{AnthToken: "rotated"})
	if err == nil || !strings.Contains(err.Error(), "cooldown") || calls != 1 {
		t.Fatalf("rotation bypassed cooldown: calls=%d err=%v", calls, err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAnthropicGateSubprocess$")
	cmd.Env = append(os.Environ(), "AIMETER_TEST_GATE_DIR="+anthropicCacheDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart bypassed cooldown: %v %s", err, out)
	}
}
func TestAnthropicGateSubprocess(t *testing.T) {
	dir := os.Getenv("AIMETER_TEST_GATE_DIR")
	if dir == "" {
		return
	}
	anthropicCacheDir = dir
	state, release, err := reserveAnthropicRequest(anthropicCache{})
	if release != nil {
		release()
		t.Fatal("new process obtained a second request")
	}
	if err != nil || !state.NextAt.After(time.Now().Add(14*time.Minute)) {
		t.Fatalf("persistent gate: %v %v", state, err)
	}
}
func TestAnthropicFailedAttemptsAreThrottled(t *testing.T) {
	for _, code := range []int{0, 401, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			isolateAnthropicCache(t)
			calls := 0
			httpc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if code == 0 {
					return nil, fmt.Errorf("offline")
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"OAuth access token has expired."}}`))}, nil
			})
			for i := 0; i < 2; i++ {
				_, _, err := fetchAnthropic(context.Background(), Creds{AnthToken: "failed"})
				if err == nil {
					t.Fatal("missing error")
				}
				if code == 401 && !anthropicExpired(err) {
					t.Fatal("lost expiry on restart")
				}
			}
			if calls != 1 {
				t.Fatalf("failed attempts=%d", calls)
			}
		})
	}
}
func TestAnthropicLongRetryAfterSurvivesTokenRotation(t *testing.T) {
	isolateAnthropicCache(t)
	calls := 0
	httpc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"3600"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, _, _ = fetchAnthropic(context.Background(), Creds{AnthToken: "first"})
	_, _, err := fetchAnthropic(context.Background(), Creds{AnthToken: "rotated"})
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "1h") {
		t.Fatalf("long cooldown lost: %d %v", calls, err)
	}
}
func TestAnthropicCooldownExpires(t *testing.T) {
	isolateAnthropicCache(t)
	path := filepath.Join(anthropicCacheDir, "anthropic-request.json")
	if err := writeAnthropicState(path, anthropicRequestState{NextAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	state, release, err := reserveAnthropicRequest(anthropicCache{})
	if err != nil || release == nil || !state.NextAt.After(time.Now().Add(14*time.Minute)) {
		t.Fatalf("expired gate: %v %v", state, err)
	}
	release()
}
func TestAnthropicConcurrentProcessesCannotReserveTwice(t *testing.T) {
	isolateAnthropicCache(t)
	_, release, err := reserveAnthropicRequest(anthropicCache{})
	if err != nil || release == nil {
		t.Fatal("first reservation failed")
	}
	defer release()
	_, second, err := reserveAnthropicRequest(anthropicCache{})
	if second != nil {
		second()
		t.Fatal("second concurrent request allowed")
	}
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("lock not enforced: %v", err)
	}
}
func TestAnthropicCacheFailurePreventsRequest(t *testing.T) {
	isolateAnthropicCache(t)
	file := filepath.Join(anthropicCacheDir, "not-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	anthropicCacheDir = file
	httpc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("request without durable cooldown")
		return nil, nil
	})
	if _, _, err := fetchAnthropic(context.Background(), Creds{AnthToken: "t"}); err == nil {
		t.Fatal("missing persistence error")
	}
}

func TestAnthropicExistingCacheMigratesWithoutRequest(t *testing.T) {
	isolateAnthropicCache(t)
	_ = writeAnthropicCache("existing", anthropicCache{FetchedAt: time.Now().Add(-10 * time.Minute), Items: []Gauge{{Label: "5h", Used: 12}}})
	httpc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("existing cache triggered early request")
		return nil, nil
	})
	gs, note, err := fetchAnthropic(context.Background(), Creds{AnthToken: "existing"})
	if err != nil || len(gs) != 1 || !strings.Contains(note, "retry in 5m") {
		t.Fatalf("cache migration: %v %q %v", gs, note, err)
	}
}
func TestAnthropicStaleLockRecoversAfterCrash(t *testing.T) {
	isolateAnthropicCache(t)
	lock := filepath.Join(anthropicCacheDir, "anthropic-request.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	ago := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(lock, ago, ago); err != nil {
		t.Fatal(err)
	}
	_, release, err := reserveAnthropicRequest(anthropicCache{})
	if err != nil || release == nil {
		t.Fatalf("stale lock not recovered: %v", err)
	}
	release()
}
