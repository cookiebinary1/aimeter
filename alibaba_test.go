package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAlibabaConsoleQuotaFlow(t *testing.T) {
	for _, china := range []bool{false, true} {
		name, baseURL := "international", alibabaInternationalURL
		if china {
			name, baseURL = "china", alibabaChinaURL
		}
		t.Run(name, func(t *testing.T) {
			previous := httpc.Transport
			calls := 0
			httpc.Transport = refreshTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("Cookie") != "session_id=test; login_aliyunid_csrf=csrf-token" || req.Header.Get("Authorization") != "" {
					t.Error("console request omitted session or sent an inference credential")
				}
				body := `{"data":{"secToken":"sec-token"}}`
				if calls == 1 {
					if req.Method != http.MethodGet {
						t.Errorf("session method = %s", req.Method)
					}
					if china {
						if req.URL.Host != "bailian.console.aliyun.com" {
							t.Errorf("China session host = %s", req.URL.Host)
						}
						body = `<script>window.CONFIG = { SEC_TOKEN: "sec-token" };</script>`
					} else if req.URL.Host != "home.qwencloud.com" {
						t.Errorf("international session host = %s", req.URL.Host)
					}
				} else {
					host, region := "cs-data.qwencloud.com", "ap-southeast-1"
					if china {
						host, region = "bailian-cs.console.aliyun.com", "cn-beijing"
					}
					if req.Method != http.MethodPost || req.URL.Host != host || req.FormValue("sec_token") != "sec-token" || req.FormValue("region") != region || req.Header.Get("x-xsrf-token") != "csrf-token" || req.Header.Get("x-csrf-token") != "csrf-token" {
						t.Error("quota request lost session, CSRF protection, or selected region")
					}
					body = `{"data":{"DataV2":{"data":{"data":{"per5HourPercentage":0.25,"per5HourResetTime":1800000000000,"per1WeekPercentage":"81.5","per1WeekResetTime":1800100000}}}}}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			t.Cleanup(func() { httpc.Transport = previous })

			gauges, _, err := fetchAlibaba(context.Background(), Creds{AliKey: "test-key", AliBaseURL: baseURL, AliCookie: "session_id=test; login_aliyunid_csrf=csrf-token"})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || len(gauges) != 2 {
				t.Fatalf("calls=%d gauges=%+v", calls, gauges)
			}
			if gauges[0].Label != "5h" || gauges[0].Used != 25 || gauges[0].Window != 5*time.Hour || gauges[0].Reset == nil || gauges[0].Reset.UnixMilli() != 1800000000000 {
				t.Errorf("five-hour quota = %+v", gauges[0])
			}
			if gauges[1].Label != "7d" || gauges[1].Used != 81.5 || gauges[1].Reset == nil || gauges[1].Reset.Unix() != 1800100000 {
				t.Errorf("weekly quota = %+v", gauges[1])
			}
		})
	}
}

func TestAlibabaMonthlyOnlyAndInvalidQuota(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{"data":{"Data":"{\"data\":{\"per1MonthPercentage\":0.0104,\"per1MonthResetTime\":1800200000000}}"}}`), &payload); err != nil {
		t.Fatal(err)
	}
	gauges, err := alibabaGauges(payload)
	if err != nil || len(gauges) != 1 {
		t.Fatalf("monthly-only result=%+v error=%v", gauges, err)
	}
	if gauges[0].Label != "monthly" || gauges[0].Used != 1.04 || gauges[0].Window != 0 || gauges[0].Reset == nil {
		t.Errorf("monthly quota = %+v; no invented duration or missing-window zeroes allowed", gauges[0])
	}
	for _, body := range []string{
		`{"successResponse":false,"data":{"per5HourPercentage":0.5}}`,
		`{"data":{"success":false,"errorCode":"NotAuthorised"}}`,
		`{"data":{"per5HourPercentage":-1,"per1WeekPercentage":"NaN"}}`,
		`{"data":{}}`,
	} {
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatal(err)
		}
		if _, err := alibabaGauges(payload); err == nil {
			t.Errorf("invalid or rejected quota accepted: %s", body)
		}
		payload = nil
	}
}

func TestAlibabaSessionRedirectDoesNotForwardCookie(t *testing.T) {
	previous := httpc.Transport
	calls := 0
	httpc.Transport = refreshTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "home.qwencloud.com" {
			t.Errorf("console credential sent to redirect host %s", req.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://other.example/login"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	t.Cleanup(func() { httpc.Transport = previous })
	_, _, err := fetchAlibaba(context.Background(), Creds{AliKey: "test-key", AliCookie: "session_id=expired"})
	if err == nil || calls != 1 {
		t.Fatalf("redirect was followed or concealed: calls=%d error=%v", calls, err)
	}
}

func TestAlibabaMissingConsoleSessionStaysVisible(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	previous := httpc.Transport
	httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
		t.Error("API-key-only credentials must not be sent to a guessed quota endpoint")
		return nil, context.Canceled
	})
	t.Cleanup(func() { httpc.Transport = previous })
	panels := fetchAll(Creds{AliKey: "test-key", AliBaseURL: alibabaInternationalURL})
	if len(panels) != 1 || panels[0].Name != "Alibaba" || panels[0].Err == nil || !strings.Contains(panels[0].Err.Error(), "ALIBABA_COOKIE") {
		t.Fatalf("missing-session explanation disappeared: %+v", panels)
	}
}

func TestAlibabaCLIQuota(t *testing.T) {
	old := runAlibabaCLI
	t.Cleanup(func() { runAlibabaCLI = old })
	for _, tc := range []struct{ base, site, region string }{
		{alibabaInternationalURL, "international", "ap-southeast-1"},
		{alibabaChinaURL, "domestic", "cn-beijing"},
	} {
		runAlibabaCLI = func(_ context.Context, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--config default") || !strings.Contains(joined, "--console-site "+tc.site) || !strings.Contains(joined, "--console-region "+tc.region) {
				t.Fatalf("wrong CLI arguments: %s", joined)
			}
			return []byte(`{"per1MonthPercentage":0.03042717518895652,"per1MonthResetTime":1795968000000,"hasQuotaData":true}`), nil
		}
		gs, note, err := fetchAlibaba(context.Background(), Creds{AliKey: "test", AliBaseURL: tc.base})
		if err != nil || len(gs) != 1 || gs[0].Used < 3.04 || gs[0].Used > 3.05 || gs[0].Reset == nil || !strings.Contains(note, "bl") {
			t.Fatalf("CLI quota: %v %q %v", gs, note, err)
		}
	}
	for _, body := range []string{`invalid`, `null`, `{"hasQuotaData":false}`, `{"error":{"message":"secret"}}`, `{}`} {
		runAlibabaCLI = func(context.Context, ...string) ([]byte, error) { return []byte(body), nil }
		if _, _, err := fetchAlibaba(context.Background(), Creds{AliKey: "test"}); err == nil {
			t.Fatalf("accepted invalid response %s", body)
		}
	}
	runAlibabaCLI = func(context.Context, ...string) ([]byte, error) { return nil, context.Canceled }
	if _, _, err := fetchAlibaba(context.Background(), Creds{AliKey: "test"}); err == nil || !strings.Contains(err.Error(), "bl auth login") {
		t.Fatalf("missing login guidance: %v", err)
	}
}
