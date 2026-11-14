package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGeminiRegionDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		raw    string
		err    error
		want   bool
	}{
		{"API error", 400, `{"error":{"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`, nil, true},
		{"escaped API error", 400, `{"error":{"message":"Gemini isn\u2019t currently supported in your country."}}`, nil, true},
		{"WebChannel status", 502, "processSession status 9: Gemini is not available in your country.", nil, true},
		{"wrapped typed error", 0, "", fmt.Errorf("fetch: %w", errGeminiRegion), true},
		{"generated region explanation", 200, refusalTestFrame("Gemini is not available in your country."), nil, false},
		{"policy refusal", 200, refusalTestFrame(refusalSample), nil, false},
		{"empty frame", 200, `[["er",null,null,1155]]`, nil, false},
		{"unknown HTTP error", 502, "Bad Gateway", nil, false},
		{"rate limit", 429, `{"error":{"message":"Resource exhausted"}}`, nil, false},
		{"timeout", 0, "", errUpstreamTimeout, false},
		{"quoted region text", 200, `{"message":"Gemini is not available in your country."}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGeminiRegionResponse(tc.status, []byte(tc.raw), tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`<html><body>Gemini isn&rsquo;t currently supported in your country. Stay tuned!</body></html>`, true},
		{`<html><body>Gemini is not available in your <b>region.</b></body></html>`, true},
		{`<script>"Gemini isn't currently supported in your country"</script><body>Hello</body>`, false},
		{`<template>Gemini isn't currently supported in your country</template>`, false},
		{`<script>"d2zJAe":"key"</script><body>Gemini isn't currently supported in your country</body>`, false},
		{`<script>"SNlM0e":"long-enough-token"</script><body>Gemini isn't currently supported in your country</body>`, false},
		{`<body>Sign in</body>`, false},
	} {
		if got := isGeminiRegionPage([]byte(tc.body)); got != tc.want {
			t.Errorf("page got %v, want %v: %s", got, tc.want, tc.body)
		}
	}
}

type fakeMihomo struct {
	mu                sync.Mutex
	now               string
	puts              int
	unauthorized      bool
	rejectPut         bool
	wrongConfirmation bool
	groupType         string
	proxies           map[string]mihomoProxy
}

func newFakeMihomo(t *testing.T) (*fakeMihomo, RuntimeConfig) {
	t.Helper()
	alive, dead := true, false
	f := &fakeMihomo{now: "old", groupType: "Selector", proxies: map[string]mihomoProxy{
		"old":      {Name: "old", Type: "VLESS", Alive: &alive},
		"dead":     {Name: "dead", Type: "VLESS", Alive: &dead},
		"DIRECT":   {Name: "DIRECT", Type: "DIRECT", Alive: &alive},
		"REJECT":   {Name: "REJECT", Type: "REJECT"},
		"subgroup": {Name: "subgroup", Type: "Selector", All: []string{"old"}},
		"next":     {Name: "next", Type: "VLESS", Alive: &alive},
		"last":     {Name: "last", Type: "VLESS", Alive: &alive},
	}}
	c := RuntimeConfig{MihomoAutoSwitch: true, MihomoGroup: "测试 / Gemini?#", MihomoProxyURL: "http://mihomo:7890", MihomoMaxSwitches: 2}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.unauthorized || r.Header.Get("Authorization") != "Bearer test-secret" {
			w.WriteHeader(401)
			return
		}
		group := mihomoProxy{Name: c.MihomoGroup, Type: f.groupType, Now: f.now, All: []string{"old", "dead", "DIRECT", "REJECT", "subgroup", "next", "last"}}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/proxies":
			pool := map[string]mihomoProxy{c.MihomoGroup: group}
			for n, p := range f.proxies {
				pool[n] = p
			}
			json.NewEncoder(w).Encode(map[string]any{"proxies": pool})
		case r.URL.Path == "/proxies/"+c.MihomoGroup && r.Method == "GET":
			json.NewEncoder(w).Encode(group)
		case r.URL.Path == "/proxies/"+c.MihomoGroup && r.Method == "PUT":
			if f.rejectPut {
				w.WriteHeader(400)
				return
			}
			var b struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			f.puts++
			if !f.wrongConfirmation {
				f.now = b.Name
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected controller operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c.MihomoController = srv.URL
	return f, c
}

func setMihomoTestConfig(t *testing.T, c RuntimeConfig) {
	t.Helper()
	old := rtCfg()
	oldSwitcher := mihomoSwitches
	proxyMu.Lock()
	oldProxies := proxyCache
	proxyCache = []Proxy{{ID: 7, URL: c.MihomoProxyURL}}
	proxyMu.Unlock()
	rtMu.Lock()
	rtVal = c
	rtMu.Unlock()
	mihomoSwitches = &mihomoSwitcher{}
	t.Setenv("MIHOMO_SECRET", "test-secret")
	t.Cleanup(func() {
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
		proxyMu.Lock()
		proxyCache = oldProxies
		proxyMu.Unlock()
		mihomoSwitches = oldSwitcher
	})
}

func TestMihomoSwitchAndConcurrentDedup(t *testing.T) {
	f, c := newFakeMihomo(t)
	s := &mihomoSwitcher{}
	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.switchNode(c, "test-secret", started); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.puts != 1 || f.now != "next" {
		t.Fatalf("concurrent switching: puts=%d now=%s", f.puts, f.now)
	}
	// A fresh failed operation may select the next node; it cannot return to old.
	if err := s.switchNode(c, "test-secret", time.Now()); err != nil {
		t.Fatal(err)
	}
	if f.puts != 2 || f.now != "last" {
		t.Fatalf("second switch: puts=%d now=%s", f.puts, f.now)
	}
	if err := s.switchNode(c, "test-secret", time.Now()); err == nil {
		t.Fatal("cycled back to a region-blocked node")
	}
}

func TestMihomoControllerFailures(t *testing.T) {
	for _, mode := range []string{"unauthorized", "put failed", "wrong confirmation", "automatic group", "cooldown", "no candidates"} {
		t.Run(mode, func(t *testing.T) {
			f, c := newFakeMihomo(t)
			s := &mihomoSwitcher{}
			switch mode {
			case "unauthorized":
				f.unauthorized = true
			case "put failed":
				f.rejectPut = true
			case "wrong confirmation":
				f.wrongConfirmation = true
			case "automatic group":
				f.groupType = "URLTest"
			case "no candidates":
				c.MihomoNodes = "missing|DIRECT|dead|subgroup"
			case "cooldown":
				if err := s.switchNode(c, "test-secret", time.Now()); err != nil {
					t.Fatal(err)
				}
				c.MihomoCooldownSec = 300
			}
			if err := s.switchNode(c, "test-secret", time.Now()); err == nil {
				t.Fatal("expected controller failure")
			}
			if f.puts > 1 {
				t.Fatal("unbounded switching on controller failure")
			}
		})
	}
}

func TestMihomoRecoveryBudgetAndRouting(t *testing.T) {
	for _, mode := range []string{"recover", "budget", "disabled", "unrelated error", "different proxy", "direct", "partial text", "partial reasoning"} {
		t.Run(mode, func(t *testing.T) {
			f, c := newFakeMihomo(t)
			if mode == "disabled" {
				c.MihomoAutoSwitch = false
			}
			setMihomoTestConfig(t, c)
			calls := 0
			res, err := generateWithMihomoRecovery(func() (*StreamResult, error) {
				calls++
				r := &StreamResult{ProxyID: 7}
				if mode == "recover" && calls == 2 {
					return r, nil
				}
				if mode == "different proxy" {
					r.ProxyID = 8
				}
				if mode == "direct" {
					r.ProxyID = 0
				}
				if mode == "partial text" {
					r.Emitted = "partial"
				}
				if mode == "partial reasoning" {
					r.EmittedReasoning = "partial"
				}
				if mode == "unrelated error" {
					return r, errUpstreamTimeout
				}
				return r, errGeminiRegion
			})
			if res == nil {
				t.Fatal("lost result attribution")
			}
			switch mode {
			case "recover":
				if calls != 2 || f.puts != 1 || err != nil {
					t.Fatalf("recovery: calls=%d puts=%d err=%v", calls, f.puts, err)
				}
			case "budget":
				if calls != 3 || f.puts != 2 || !errors.Is(err, errGeminiRegion) {
					t.Fatalf("budget: calls=%d puts=%d err=%v", calls, f.puts, err)
				}
			default:
				if calls != 1 || f.puts != 0 {
					t.Fatalf("unexpected retry: calls=%d puts=%d", calls, f.puts)
				}
			}
		})
	}
}

func TestMihomoCandidateAllowlistAndExpiry(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoNodes = "last | next"
	s := &mihomoSwitcher{}
	if err := s.switchNode(c, "test-secret", time.Now()); err != nil {
		t.Fatal(err)
	}
	if f.now != "last" {
		t.Fatal("ignored candidate order")
	}
	s.blocked["next"] = time.Now().Add(-time.Second)
	if err := s.switchNode(c, "test-secret", time.Now()); err != nil {
		t.Fatal(err)
	}
	if f.now != "next" {
		t.Fatal("did not expire blocked node")
	}
}

func TestMihomoConfigAndCredentialIsolation(t *testing.T) {
	_, c := newFakeMihomo(t)
	if err := validateMihomoConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, controller := range []string{"", "ftp://host", "http://secret@host", "http://host?secret=value", "http://host#secret"} {
		bad := c
		bad.MihomoController = controller
		if validateMihomoConfig(bad) == nil {
			t.Fatalf("accepted invalid controller %q", controller)
		}
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "mihomo_secret") {
		t.Fatal("runtime config leaks credential")
	}
	if defaultConfig().MihomoAutoSwitch {
		t.Fatal("feature enabled by default")
	}
	oldSecret := cfg.MihomoSecret
	t.Cleanup(func() { cfg.MihomoSecret = oldSecret })
	cfg.MihomoSecret = "config-secret"
	t.Setenv("MIHOMO_SECRET", "env-secret")
	if mihomoSecret() != "env-secret" {
		t.Fatal("environment credential not used")
	}
}
