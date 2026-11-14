package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMihomoRecoveryPrefersMeasuredLatency(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoPreferLowLatency = true
	for name, delay := range map[string]int{"next": 120, "last": 30} {
		p := f.proxies[name]
		p.History = append(p.History, struct {
			Delay int       `json:"delay"`
			Time  time.Time `json:"time"`
		}{Delay: delay})
		f.proxies[name] = p
	}
	s := &mihomoSwitcher{}
	if err := s.switchNode(c, "test-secret", s.generation.Load()); err != nil {
		t.Fatal(err)
	}
	if f.now != "last" {
		t.Fatalf("selected %s, expected lowest measured latency", f.now)
	}
}

func TestMihomoOptimizationFallbackAndBlock(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoProbeTimeoutSec, c.MihomoOptimizeIntervalMin = 5, 5
	f.delays = map[string]int{"DIRECT": 1, "subgroup": 2, "old": 90, "last": 10, "next": 20}
	s := &mihomoSwitcher{}
	var seen []string
	r := s.optimize(context.Background(), c, "test-secret", func(context.Context, RuntimeConfig) error {
		seen = append(seen, f.now)
		if f.now == "last" {
			return errGeminiRegion
		}
		return nil
	})
	if !r.OK || r.Node != "next" || r.DelayMS != 20 || r.Attempts != 2 || strings.Join(seen, ",") != "last,next" {
		t.Fatalf("unexpected result: %+v, probes=%v", r, seen)
	}
	if !s.blocked["last"].After(time.Now().Add(29 * time.Minute)) {
		t.Fatal("region failure was not cooled down")
	}
	r = s.optimize(context.Background(), c, "test-secret", func(context.Context, RuntimeConfig) error { return nil })
	if !r.OK || r.Node != "next" || f.puts != 2 {
		t.Fatalf("fastest healthy current node should not be reselected: %+v, puts=%d", r, f.puts)
	}
}

func TestMihomoOptimizationRestoresOnAllFailures(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoProbeTimeoutSec, c.MihomoOptimizeIntervalMin = 5, 5
	f.delays = map[string]int{"last": 10, "next": 20, "old": 90}
	r := (&mihomoSwitcher{}).optimize(context.Background(), c, "test-secret", func(context.Context, RuntimeConfig) error { return errors.New("failed") })
	if r.OK || r.Node != "old" || f.now != "old" || f.puts != 3 || r.Attempts != 2 {
		t.Fatalf("original node was not restored: %+v puts=%d", r, f.puts)
	}
}

func TestMihomoOptimizationReportsUnconfirmedRestoration(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoProbeTimeoutSec = 5
	f.delays = map[string]int{"last": 10}
	f.rejectPut = true
	r := (&mihomoSwitcher{}).optimize(context.Background(), c, "test-secret", func(context.Context, RuntimeConfig) error {
		t.Fatal("selection failed, must not probe")
		return nil
	})
	if r.OK || r.Reason != "restore_failed" || r.Node != "" {
		t.Fatalf("claimed a selected node without confirmation: %+v", r)
	}
}

func TestMihomoOptimizationRestoresAfterCancellation(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoProbeTimeoutSec, c.MihomoOptimizeIntervalMin = 5, 5
	f.delays = map[string]int{"last": 10, "old": 90}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := (&mihomoSwitcher{}).optimize(ctx, c, "test-secret", func(context.Context, RuntimeConfig) error { cancel(); return ctx.Err() })
	if r.OK || r.Node != "old" || f.now != "old" {
		t.Fatalf("cancelled run did not restore: %+v", r)
	}
}

func TestMihomoOptimizationHonorsCandidatesAndZeroDelay(t *testing.T) {
	f, c := newFakeMihomo(t)
	c.MihomoProbeTimeoutSec, c.MihomoOptimizeIntervalMin = 5, 5
	c.MihomoNodes = "last | next"
	f.delays = map[string]int{"last": 0, "old": 1, "next": 30}
	r := (&mihomoSwitcher{}).optimize(context.Background(), c, "test-secret", func(context.Context, RuntimeConfig) error { return nil })
	if !r.OK || r.Node != "next" {
		t.Fatalf("selected unreachable or unlisted node: %+v", r)
	}
}

func TestMihomoOptimizationDoesNotSwitchWithoutMeasurements(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "all unreachable", true: "controller failed"}[failed], func(t *testing.T) {
			f, c := newFakeMihomo(t)
			c.MihomoProbeTimeoutSec = 5
			f.delayFailed = failed
			r := (&mihomoSwitcher{}).optimize(context.Background(), c, "test-secret", func(context.Context, RuntimeConfig) error { t.Fatal("must not probe"); return nil })
			if r.OK || f.puts != 0 {
				t.Fatalf("modified selector without usable latency: %+v", r)
			}
		})
	}
}

func TestMihomoAnonymousProbeHasNoCredentialsAndChecksBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"reply", 200, okRaw, true},
		{"region", 200, legacyRegionRaw, false},
		{"empty", 200, rejectedRaw, false},
		{"redirect", 302, "", false},
		{"throttle", 429, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
					t.Fatal("anonymous probe contained credentials")
				}
				if r.URL.Host != "gemini.invalid" {
					t.Fatalf("unexpected endpoint %s", r.URL.Host)
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("at") != "" || r.Form.Get("f.req") == "" {
					t.Fatal("invalid anonymous form")
				}
				w.Header().Set("Location", "http://must-not-follow.invalid")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer proxy.Close()
			c := RuntimeConfig{MihomoProxyURL: proxy.URL, MihomoProbeTimeoutSec: 5}
			err := probeMihomoAnonymousAt(context.Background(), c, "http://gemini.invalid/generate")
			if (err == nil) != tc.ok || count != 1 {
				t.Fatalf("result=%v calls=%d", err, count)
			}
		})
	}
}

func TestMihomoOptimizationSchedule(t *testing.T) {
	s := mihomoOptimizationSchedule{}
	now := time.Now()
	c := RuntimeConfig{MihomoOptimizeIntervalMin: 5}
	if s.due(c, now) {
		t.Fatal("disabled optimization must not run")
	}
	c.MihomoAutoOptimize = true
	if !s.due(c, now) {
		t.Fatal("first enabled run should be due")
	}
	s.Next = now.Add(5 * time.Minute)
	c.DefaultModel = "unrelated-model-change"
	if s.due(c, now.Add(4*time.Minute)) {
		t.Fatal("unrelated setting must not cause another probe")
	}
	if !s.due(c, now.Add(5*time.Minute)) {
		t.Fatal("scheduled run not due")
	}
	c.MihomoAutoOptimize = false
	s.due(c, now)
	c.MihomoAutoOptimize = true
	if !s.due(c, now) {
		t.Fatal("re-enable should start a new schedule")
	}
}

func TestMihomoDedicatedConfigAndOldSettingsPreserveValues(t *testing.T) {
	f, c := newFakeMihomo(t)
	_ = f
	c.RetryAttempts, c.RequestTimeout, c.RetentionDays = 1, 20, 30
	c.DefaultModel, c.Impersonate, c.GeminiBL = "gemini-3.6-flash", "chrome_146", defaultConfig().GeminiBL
	c.MihomoOptimizeIntervalMin, c.MihomoProbeTimeoutSec = 5, 20
	setMihomoTestConfig(t, c)
	previousSaved := kvGet(runtimeConfigKey)
	t.Cleanup(func() { kvSet(runtimeConfigKey, previousSaved) })
	request := httptest.NewRequest("PUT", "/admin/api/mihomo/config", strings.NewReader(`{"mihomo_prefer_low_latency":true,"mihomo_auto_optimize":true}`))
	w := httptest.NewRecorder()
	handleAdminMihomoConfig(w, request)
	if w.Code != 200 {
		t.Fatalf("dedicated config failed: %s", w.Body.String())
	}
	after := rtCfg()
	if !after.MihomoAutoOptimize || !after.MihomoPreferLowLatency || after.RequestTimeout != c.RequestTimeout || after.DefaultModel != c.DefaultModel {
		t.Fatal("dedicated save changed unrelated settings")
	}
	oldPage := httptest.NewRequest("PUT", "/admin/api/config", strings.NewReader(`{"request_timeout_sec":25}`))
	w = httptest.NewRecorder()
	handleAdminConfig(w, oldPage)
	if w.Code != 200 || !rtCfg().MihomoAutoOptimize || rtCfg().RequestTimeout != 25 {
		t.Fatalf("old settings page reset optimization: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "test-secret") {
		t.Fatal("config response leaked secret")
	}
	bad := httptest.NewRequest("PUT", "/admin/api/mihomo/config", strings.NewReader(`{"anon_first":true}`))
	w = httptest.NewRecorder()
	handleAdminMihomoConfig(w, bad)
	if w.Code != 400 {
		t.Fatal("dedicated page accepted unrelated field")
	}
}
