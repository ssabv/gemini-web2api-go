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
		{"legacy region error", 200, legacyRegionRaw, nil, true},
		{"legacy code without message", 200, strings.ReplaceAll(legacyRegionRaw, `,null,"Gemini isn't available in your region yet. (1060)"`, ""), nil, true},
		{"legacy changed code with region message", 200, strings.ReplaceAll(legacyRegionRaw, "[1060,", "[9999,"), nil, true},
		{"legacy unrelated detail type", 200, strings.ReplaceAll(legacyRegionRaw, "BardErrorInfo", "UnrelatedInfo"), nil, false},
		{"legacy ordinary empty reply", 200, rejectedRaw, nil, false},
		{"legacy region trailer after generated content", 200, okRaw + legacyRegionRaw, nil, false},
		{"malformed legacy status", 200, `[["wrb.fr",null,null,null,null,[9,null,{"code":1060}]]]`, nil, false},
		{"incomplete legacy status", 200, `[["wrb.fr",null,null,null,null,[9,null]]]`, nil, false},
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

// Reproduce the real service error with synthetic timing/response metadata.
const legacyRegionRaw = ")]}'\n\n200\n" +
	`[["wrb.fr",null,null,null,null,[9,null,[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1060,null,"Gemini isn't available in your region yet. (1060)"]]]]]]` + "\n"

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
	for _, mode := range []string{"recover", "legacy region", "legacy empty", "budget", "disabled", "unrelated error", "different proxy", "direct", "partial text", "partial reasoning"} {
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
				if (mode == "recover" || mode == "legacy region") && calls == 2 {
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
				if mode == "legacy region" || mode == "legacy empty" {
					raw := legacyRegionRaw
					if mode == "legacy empty" {
						raw = rejectedRaw
					}
					if !isGeminiRegionResponse(200, []byte(raw), nil) {
						return r, errors.New("no content frame")
					}
				}
				return r, errGeminiRegion
			})
			if res == nil {
				t.Fatal("lost result attribution")
			}
			switch mode {
			case "recover", "legacy region":
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

func isolateMihomoPanelSecret(t *testing.T) {
	t.Helper()
	oldKV := kvGet(mihomoSecretKey)
	mihomoSecretMu.Lock()
	oldPanel := mihomoPanelSecret
	mihomoPanelSecret = ""
	mihomoSecretMu.Unlock()
	oldAdmin := cfg.AdminToken
	if err := kvSet(mihomoSecretKey, ""); err != nil {
		t.Fatal(err)
	}
	cfg.AdminToken = "admin-test-token"
	t.Cleanup(func() {
		kvSet(mihomoSecretKey, oldKV)
		mihomoSecretMu.Lock()
		mihomoPanelSecret = oldPanel
		mihomoSecretMu.Unlock()
		cfg.AdminToken = oldAdmin
	})
}

func TestMihomoPanelSecretPersistenceAndNoDisclosure(t *testing.T) {
	isolateMihomoPanelSecret(t)
	t.Setenv("MIHOMO_SECRET", "environment-test-password")
	body := `{"secret":"panel-test-password"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/admin/api/mihomo/secret", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-test-token")
	requireAuth(handleAdminMihomoSecret)(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "panel-test-password") {
		t.Fatalf("secret save response: %d %s", w.Code, w.Body.String())
	}
	if mihomoSecret() != "panel-test-password" {
		t.Fatal("panel password did not override environment")
	}
	// Clearing only the in-memory value simulates startup loading from persistence.
	mihomoSecretMu.Lock()
	mihomoPanelSecret = ""
	mihomoSecretMu.Unlock()
	initMihomoSecret()
	if mihomoSecret() != "panel-test-password" {
		t.Fatal("saved password did not survive restart")
	}
	w = httptest.NewRecorder()
	handleAdminConfig(w, httptest.NewRequest("GET", "/admin/api/config", nil))
	if strings.Contains(w.Body.String(), "panel-test-password") || strings.Contains(w.Body.String(), "environment-test-password") {
		t.Fatal("config read disclosed secret")
	}
	var got struct {
		Deploy struct {
			Set   bool `json:"mihomo_secret_set"`
			Panel bool `json:"mihomo_secret_from_panel"`
		} `json:"deploy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !got.Deploy.Set || !got.Deploy.Panel {
		t.Fatal("missing password status")
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("PUT", "/admin/api/mihomo/secret", strings.NewReader(`{"secret":""}`))
	r.Header.Set("Authorization", "Bearer admin-test-token")
	requireAuth(handleAdminMihomoSecret)(w, r)
	initMihomoSecret()
	if w.Code != 200 || mihomoPanelSecretSet() || mihomoSecret() != "environment-test-password" {
		t.Fatal("clearing override did not restore environment")
	}
}

func TestMihomoPanelSecretRequiresExplicitAuthenticatedWrite(t *testing.T) {
	isolateMihomoPanelSecret(t)
	for _, tc := range []struct {
		name, method, body string
		auth               bool
		status             int
	}{
		{"unauthorized", "PUT", `{"secret":"rejected-password"}`, false, 401},
		{"read", "GET", "", true, 405},
		{"missing field", "PUT", `{}`, true, 400},
		{"malformed", "PUT", `{"secret":"rejected-password"`, true, 400},
		{"invalid header", "PUT", `{"secret":"rejected\npassword"}`, true, 400},
		{"too long", "PUT", `{"secret":"` + strings.Repeat("x", 4097) + `"}`, true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, "/admin/api/mihomo/secret", strings.NewReader(tc.body))
			if tc.auth {
				r.Header.Set("Authorization", "Bearer admin-test-token")
			}
			requireAuth(handleAdminMihomoSecret)(w, r)
			if w.Code != tc.status || mihomoPanelSecretSet() || kvGet(mihomoSecretKey) != "" {
				t.Fatalf("rejected request changed password: status %d", w.Code)
			}
			if strings.Contains(w.Body.String(), "rejected-password") {
				t.Fatal("error disclosed attempted password")
			}
		})
	}
}

func TestMihomoGroupDiscoveryReadOnlyAndSecretIsolation(t *testing.T) {
	isolateMihomoPanelSecret(t)
	if err := saveMihomoSecret("saved-test-password"); err != nil {
		t.Fatal(err)
	}
	oldRuntime := rtCfg()
	requests := 0
	wantSecret := "saved-test-password"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/proxies" || r.Header.Get("Authorization") != "Bearer "+wantSecret {
			t.Error("unexpected controller request")
			w.WriteHeader(401)
			return
		}
		body := `{"proxies":{"z 手动":{"type":"Selector","now":"node-a","all":["node-a","node-b"],"password":"hidden-node-password"},"a 自动":{"type":"URLTest","now":"node-a","all":["node-a"]},"空组":{"type":"Selector","all":[]},"node-a":{"type":"Vless","server":"private-node-server"}},"secret":"hidden-controller-password"}`
		w.Write([]byte(body))
	}))
	defer srv.Close()
	for _, temporary := range []bool{false, true} {
		body := map[string]any{"controller": srv.URL}
		if temporary {
			wantSecret = "temporary-test-password"
			body["secret"] = wantSecret
		}
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/admin/api/mihomo/groups", strings.NewReader(string(encoded)))
		r.Header.Set("Authorization", "Bearer admin-test-token")
		w := httptest.NewRecorder()
		requireAuth(handleAdminMihomoGroups)(w, r)
		if w.Code != 200 {
			t.Fatalf("discovery failed: %d %s", w.Code, w.Body.String())
		}
		for _, hidden := range []string{"password", "private-node-server", "Vless", "server"} {
			if strings.Contains(w.Body.String(), hidden) {
				t.Fatal("discovery disclosed controller or node configuration")
			}
		}
		var got struct {
			Groups []mihomoGroup `json:"groups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Groups) != 3 {
			t.Fatalf("unexpected group list: %s", w.Body.String())
		}
		if got.Groups[0].Name != "a 自动" || got.Groups[0].Selectable || got.Groups[1].Name != "z 手动" ||
			!got.Groups[1].Selectable || got.Groups[1].NodeCount != 2 || got.Groups[1].Now != "node-a" {
			t.Fatal("incorrect group metadata or ordering")
		}
		if mihomoSecret() != "saved-test-password" || kvGet(mihomoSecretKey) != "saved-test-password" || rtCfg() != oldRuntime {
			t.Fatal("discovery modified credentials or runtime configuration")
		}
	}
	if requests != 2 {
		t.Fatal("unexpected number of controller requests")
	}
}

func TestMihomoGroupDiscoveryRejectsInvalidAndUnauthenticatedRequests(t *testing.T) {
	isolateMihomoPanelSecret(t)
	for _, tc := range []struct {
		method, body string
		auth         bool
		status       int
	}{
		{"POST", `{"controller":"http://controller"}`, false, 401},
		{"GET", "", true, 405},
		{"POST", `{`, true, 400},
		{"POST", `{}`, true, 400},
		{"POST", `{"controller":"ftp://controller"}`, true, 400},
		{"POST", `{"controller":"http://user:attempted-password@controller"}`, true, 400},
		{"POST", `{"controller":"http://controller?secret=attempted-password"}`, true, 400},
		{"POST", `{"controller":"http://controller#attempted-password"}`, true, 400},
		{"POST", `{"controller":"http://controller","secret":"attempted-password\n"}`, true, 400},
		{"POST", `{"controller":"http://controller","secret":"` + strings.Repeat("x", 4097) + `"}`, true, 400},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, "/admin/api/mihomo/groups", strings.NewReader(tc.body))
		if tc.auth {
			r.Header.Set("Authorization", "Bearer admin-test-token")
		}
		requireAuth(handleAdminMihomoGroups)(w, r)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "attempted-password") || mihomoPanelSecretSet() {
			t.Fatalf("unexpected rejection response: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestMihomoGroupDiscoveryControllerFailuresAndEmptyList(t *testing.T) {
	isolateMihomoPanelSecret(t)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"unauthorized", 401, "hidden-upstream-password", 502},
		{"invalid JSON", 200, "hidden-upstream-password", 502},
		{"empty", 200, `{"proxies":{}}`, 200},
		{"redirect", 302, "hidden-upstream-password", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/proxies" {
					t.Error("discovery followed a redirect or changed nodes")
				}
				w.Header().Set("Location", "/not-allowed")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			body, _ := json.Marshal(map[string]string{"controller": srv.URL})
			r := httptest.NewRequest("POST", "/admin/api/mihomo/groups", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer admin-test-token")
			w := httptest.NewRecorder()
			requireAuth(handleAdminMihomoGroups)(w, r)
			if w.Code != tc.want || strings.Contains(w.Body.String(), "hidden-upstream-password") {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
			}
			if tc.want == 200 && strings.TrimSpace(w.Body.String()) != `{"groups":[]}` {
				t.Fatal("empty list must be a JSON array")
			}
		})
	}
}
