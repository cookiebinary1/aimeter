package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestJSONPointer(t *testing.T) {
	doc := map[string]any{
		"data": map[string]any{
			"limits": []any{
				map[string]any{"used": 42.0},
				map[string]any{"used": 99.0},
			},
			"plan~name": "pro",
			"a/b":       "slash",
		},
	}
	cases := []struct {
		ptr  string
		want any
		ok   bool
	}{
		{"", doc, true},
		{"/data/limits/0/used", 42.0, true},
		{"/data/limits/1/used", 99.0, true},
		{"/data/plan~0name", "pro", true},
		{"/data/a~1b", "slash", true},
		{"/data/limits/2", nil, false},
		{"/data/limits/x", nil, false},
		{"/data/missing", nil, false},
		{"/data/limits/0/used/deep", nil, false},
	}
	for _, c := range cases {
		got, ok := jsonPointer(doc, c.ptr)
		if ok != c.ok {
			t.Errorf("jsonPointer(%q): ok=%v, want %v", c.ptr, ok, c.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("jsonPointer(%q): got %#v, want %#v", c.ptr, got, c.want)
		}
	}
}

func TestFetchCustom(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":{"used":25,"quota":100,"plan":"pro"},"pct":75.5}`))
	}))
	defer srv.Close()

	p := customProvider{
		Name:   "TestAI",
		URL:    srv.URL,
		APIKey: "sk-test",
		Used:   "/data/used",
		Total:  "/data/quota",
		Detail: "/data/plan",
	}
	gs, _, err := fetchCustom(context.Background(), p)
	if err != nil {
		t.Fatalf("fetchCustom: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization: %q, want Bearer sk-test", gotAuth)
	}
	if len(gs) != 1 || gs[0].Used != 25 || gs[0].Label != "usage" || gs[0].Detail != "pro" {
		t.Errorf("gauge: %+v", gs[0])
	}

	// percent mode + custom label
	p2 := p
	p2.Percent, p2.Used, p2.Total, p2.Label = "/pct", "", "", "monthly"
	gs, _, err = fetchCustom(context.Background(), p2)
	if err != nil || gs[0].Used != 75.5 || gs[0].Label != "monthly" {
		t.Errorf("percent mode: gs=%+v err=%v", gs, err)
	}

	// missing pointer must error, not render garbage
	p3 := p
	p3.Used = "/nope/used"
	if _, _, err := fetchCustom(context.Background(), p3); err == nil {
		t.Error("missing pointer should error")
	}

	// neither percent nor used+total
	p4 := customProvider{Name: "X", URL: srv.URL, APIKey: "k"}
	if _, _, err := fetchCustom(context.Background(), p4); err == nil {
		t.Error("no pointers should error")
	}

	// missing api_key
	p5 := p
	p5.APIKey = ""
	if _, _, err := fetchCustom(context.Background(), p5); err == nil {
		t.Error("missing api_key should error")
	}
}
