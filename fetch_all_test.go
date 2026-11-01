package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetchAllMissingCredentials(t *testing.T) {
	var calls atomic.Int32
	previous := httpc.Transport
	httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected request")
	})
	t.Cleanup(func() { httpc.Transport = previous })

	panels := fetchAll(Creds{})
	if len(panels) != 0 {
		t.Fatalf("panels = %+v, want no unconfigured providers", panels)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("requests = %d, want none", got)
	}
	encoded, err := json.Marshal(panels)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Errorf("JSON = %s, want []", encoded)
	}
}

func TestFetchAllConfiguredHTTPFailure(t *testing.T) {
	var calls atomic.Int32
	previous := httpc.Transport
	httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"forbidden"}`)),
		}, nil
	})
	t.Cleanup(func() { httpc.Transport = previous })

	panels := fetchAll(Creds{OpenRouter: "test-key"})
	if len(panels) != 1 || panels[0].Name != "OpenRouter" || panels[0].Err == nil {
		t.Fatalf("panels = %+v, want visible OpenRouter failure", panels)
	}
	var status *httpStatusError
	if !errors.As(panels[0].Err, &status) || status.code != http.StatusForbidden {
		t.Errorf("error = %v, want HTTP 403", panels[0].Err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want only the configured provider", got)
	}
}

func TestFetchAllCustomFailureRemainsVisible(t *testing.T) {
	panels := fetchAll(Creds{Custom: []customProvider{{Name: "Custom failure"}}})
	if len(panels) != 1 || panels[0].Name != "Custom failure" || panels[0].Err == nil {
		t.Fatalf("panels = %+v, want visible custom configuration failure", panels)
	}
}

func TestFetchAllPreservesConfiguredOrder(t *testing.T) {
	withStub(t, `{"data":{"total_credits":10,"total_usage":1},"tier":"free","character_count":1,"character_limit":10,"percent":25}`)
	panels := fetchAll(Creds{
		OpenRouter: "test-key",
		EleKey:     "test-key",
		Custom: []customProvider{
			{Name: "Custom second", URL: "https://example.invalid/second", APIKey: "test-key", Percent: "/percent"},
			{Name: "Custom first", URL: "https://example.invalid/first", APIKey: "test-key", Percent: "/percent"},
		},
	})
	want := []string{"OpenRouter", "ElevenLabs", "Custom second", "Custom first"}
	if len(panels) != len(want) {
		t.Fatalf("panels = %+v, want %d configured providers", panels, len(want))
	}
	for i, name := range want {
		if panels[i].Name != name || panels[i].Err != nil {
			t.Errorf("panel %d = %+v, want successful %s", i, panels[i], name)
		}
	}
}
