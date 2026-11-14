package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func mihomoLeaf(p mihomoProxy) bool {
	if len(p.All) != 0 || p.Type == "" {
		return false
	}
	switch strings.ToLower(p.Type) {
	case "selector", "urltest", "fallback", "loadbalance", "relay", "direct", "reject", "rejectdrop", "pass", "dns", "compatible":
		return false
	}
	return true
}

func mihomoDelay(p mihomoProxy) int {
	if len(p.History) != 0 {
		return p.History[len(p.History)-1].Delay
	}
	return 0
}

func sortMihomoByLatency(names []string, proxies map[string]mihomoProxy) {
	sort.SliceStable(names, func(i, j int) bool {
		a, b := mihomoDelay(proxies[strings.TrimSpace(names[i])]), mihomoDelay(proxies[strings.TrimSpace(names[j])])
		if a <= 0 {
			return false
		}
		return b <= 0 || a < b
	})
}

func closeMihomoIdleConnections(proxyURL string) {
	stdlibClientCache.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), proxyURL+"|") {
			v.(*http.Client).CloseIdleConnections()
		}
		return true
	})
}

// Dedicated transport, no account lookup, cookies, authentication or retry. Only a
// genuine generated reply counts as success; HTTP 200 alone is insufficient.
func probeMihomoAnonymous(ctx context.Context, c RuntimeConfig) error {
	u := "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=" + url.QueryEscape(c.GeminiBL) + "&hl=en&rt=c&_reqid=" + fmt.Sprint(time.Now().Unix()%1000000)
	return probeMihomoAnonymousAt(ctx, c, u)
}

func probeMihomoAnonymousAt(ctx context.Context, c RuntimeConfig, endpoint string) error {
	proxy, err := url.Parse(c.MihomoProxyURL)
	if err != nil || proxy.Host == "" {
		return errors.New("anonymous probe proxy is invalid")
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Duration(c.MihomoProbeTimeoutSec) * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	mc := Models["gemini-3.6-flash"]
	inner := make([]any, innerSlots)
	inner[0] = []any{"Reply with OK only.", 0, nil, nil, nil, nil, 0}
	inner[1] = []any{"en"}
	inner[2] = []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6], inner[7], inner[10], inner[11] = []any{0}, 1, 1, 0
	inner[17], inner[18], inner[27], inner[30] = []any{[]any{0}}, 0, 1, []any{4}
	inner[41], inner[53], inner[61], inner[68], inner[79] = []any{1}, 0, []any{}, 1, mc.Mode
	id := uuid.NewString()
	inner[59], inner[80], inner[91], inner[96] = id, thinkingNormal, 0, 0
	rawInner, _ := json.Marshal(inner)
	rawOuter, _ := json.Marshal([]any{nil, string(rawInner)})
	form := url.Values{"f.req": {string(rawOuter)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("anonymous probe request is invalid")
	}
	applyChromeHeaders(req)
	for k, v := range buildGeminiHeaders("", "", mc.HexID) {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-goog-ext-525001261-jspb", buildModelHeader(mc.HexID, mc.Mode, thinkingNormal, uuid.NewString()))
	req.Header.Set("x-goog-ext-525005358-jspb", fmt.Sprintf(`["%s",1]`, id))
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("anonymous probe network or timeout error")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return errors.New("anonymous probe response is incomplete")
	}
	if isGeminiRegionResponse(resp.StatusCode, body, nil) || isGeminiRegionPage(body) {
		return errGeminiRegion
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anonymous probe HTTP %d", resp.StatusCode)
	}
	if extractResponseText(string(body)) == "" {
		return errors.New("anonymous probe has no generated reply")
	}
	for _, line := range strings.Split(string(body), "\n") {
		if failureReplyInLine(line) {
			return errors.New("anonymous probe was rejected upstream")
		}
	}
	return nil
}

type mihomoOptimizationResult struct {
	CheckedAt    int64  `json:"checked_at"`
	OK           bool   `json:"ok"`
	PreviousNode string `json:"previous_node"`
	Node         string `json:"node"`
	DelayMS      int    `json:"delay_ms"`
	ProbeMS      int64  `json:"probe_ms"`
	ProbeError   string `json:"probe_error,omitempty"`
	Attempts     int    `json:"attempts"`
	Reason       string `json:"reason,omitempty"`
}

var mihomoOptimizerState struct {
	sync.Mutex
	Running bool
	NextRun int64
	History []mihomoOptimizationResult
}

func performMihomoOptimization(ctx context.Context, c RuntimeConfig) mihomoOptimizationResult {
	mihomoOptimizerState.Lock()
	if mihomoOptimizerState.Running {
		mihomoOptimizerState.Unlock()
		return mihomoOptimizationResult{Reason: "optimization_running"}
	}
	mihomoOptimizerState.Running = true
	mihomoOptimizerState.Unlock()
	result := mihomoSwitches.optimize(ctx, c, mihomoSecret(), probeMihomoAnonymous)
	result.CheckedAt = time.Now().Unix()
	mihomoOptimizerState.Lock()
	mihomoOptimizerState.Running = false
	mihomoOptimizerState.History = append(mihomoOptimizerState.History, result)
	if len(mihomoOptimizerState.History) > 20 {
		mihomoOptimizerState.History = mihomoOptimizerState.History[1:]
	}
	mihomoOptimizerState.Unlock()
	return result
}

func (s *mihomoSwitcher) optimize(ctx context.Context, c RuntimeConfig, secret string, probe func(context.Context, RuntimeConfig) error) (result mihomoOptimizationResult) {
	if validateMihomoConfig(c) != nil || c.MihomoProbeTimeoutSec < 5 {
		result.Reason = "invalid_configuration"
		return
	}
	if !s.mu.TryLock() {
		result.Reason = "switch_in_progress"
		return
	}
	defer s.mu.Unlock()
	key := c.MihomoController + "\n" + c.MihomoGroup
	if s.key != key {
		s.key, s.lastSwitch, s.lastNode, s.blocked = key, time.Time{}, "", map[string]time.Time{}
	}
	if time.Since(s.lastSwitch) < time.Duration(c.MihomoCooldownSec)*time.Second {
		result.Reason = "switch_cooldown"
		return
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var pool struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	if mihomoRequestContext(ctx, client, c, secret, "GET", "/proxies", nil, &pool) != nil {
		result.Reason = "controller_unavailable"
		return
	}
	group, ok := pool.Proxies[c.MihomoGroup]
	if !ok || group.Type != "Selector" || group.Now == "" {
		result.Reason = "invalid_group"
		return
	}
	result.PreviousNode, result.Node = group.Now, group.Now
	// The controller tests nodes without changing the selected group. A fresh zero
	// or missing delay means unreachable; never mistake it for the fastest node.
	var delays map[string]int
	path := "/group/" + url.PathEscape(c.MihomoGroup) + "/delay?url=" + url.QueryEscape("https://www.gstatic.com/generate_204") + "&timeout=5000"
	if mihomoRequestContext(ctx, client, c, secret, "GET", path, nil, &delays) != nil {
		result.Reason = "latency_test_failed"
		return
	}
	allowed := map[string]bool{}
	if strings.TrimSpace(c.MihomoNodes) != "" {
		for _, n := range strings.FieldsFunc(c.MihomoNodes, func(r rune) bool { return r == '\n' || r == '\r' || r == '|' }) {
			allowed[strings.TrimSpace(n)] = true
		}
	}
	names := make([]string, 0, len(group.All))
	for _, n := range group.All {
		if mihomoLeaf(pool.Proxies[n]) && delays[n] > 0 && !s.blocked[n].After(time.Now()) && (len(allowed) == 0 || allowed[n]) {
			names = append(names, n)
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return delays[names[i]] < delays[names[j]] })
	selectNode := func(selectCtx context.Context, name string) error {
		body, _ := json.Marshal(map[string]string{"name": name})
		if err := mihomoRequestContext(selectCtx, client, c, secret, "PUT", "/proxies/"+url.PathEscape(c.MihomoGroup), strings.NewReader(string(body)), nil); err != nil {
			return err
		}
		var selected mihomoProxy
		if err := mihomoRequestContext(selectCtx, client, c, secret, "GET", "/proxies/"+url.PathEscape(c.MihomoGroup), nil, &selected); err != nil {
			return err
		}
		if selected.Now != name {
			return errors.New("node selection not confirmed")
		}
		s.lastSwitch, s.lastNode = time.Now(), name
		s.generation.Add(1)
		result.Node = name
		closeMihomoIdleConnections(c.MihomoProxyURL)
		return nil
	}
	changed := false
	defer func() {
		if changed && !result.OK {
			// Restore even when the caller's probe deadline has expired.
			restoreCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if selectNode(restoreCtx, group.Now) != nil {
				result.Node = ""
				result.Reason = "restore_failed"
			}
		}
	}()
	result.Reason = "no_eligible_node"
	for _, name := range names {
		if result.Attempts >= c.MihomoMaxSwitches || ctx.Err() != nil {
			break
		}
		if name != result.Node {
			changed = true
			if selectNode(ctx, name) != nil {
				result.Reason = "selection_failed"
				return
			}
		}
		result.Attempts++
		start := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, time.Duration(c.MihomoProbeTimeoutSec)*time.Second)
		err := probe(probeCtx, c)
		cancel()
		result.ProbeMS = time.Since(start).Milliseconds()
		if err == nil {
			result.OK, result.DelayMS, result.Reason = true, delays[name], ""
			result.ProbeError = ""
			logf("[mihomo] anonymous check succeeded: group=%q node=%q delay=%dms", c.MihomoGroup, name, result.DelayMS)
			return
		}
		result.ProbeError = err.Error()
		logf("[mihomo] anonymous check failed: group=%q node=%q error=%s", c.MihomoGroup, name, result.ProbeError)
		until := time.Now().Add(time.Duration(c.MihomoOptimizeIntervalMin) * time.Minute)
		if errors.Is(err, errGeminiRegion) {
			until = time.Now().Add(30 * time.Minute)
		}
		s.blocked[name] = until
		result.Reason = "anonymous_check_failed"
	}
	logf("[mihomo] optimization stopped: group=%q reason=%s attempts=%d", c.MihomoGroup, result.Reason, result.Attempts)
	return
}

type mihomoOptimizationSchedule struct {
	Next time.Time
	Key  string
}

func (s *mihomoOptimizationSchedule) due(c RuntimeConfig, now time.Time) bool {
	if !c.MihomoAutoOptimize {
		s.Key, s.Next = "", time.Time{}
		return false
	}
	key := fmt.Sprintf("%s|%s|%s|%s|%d|%d|%d|%d", c.MihomoController, c.MihomoGroup, c.MihomoProxyURL, c.MihomoNodes, c.MihomoOptimizeIntervalMin, c.MihomoProbeTimeoutSec, c.MihomoMaxSwitches, c.MihomoCooldownSec)
	if key != s.Key {
		s.Key, s.Next = key, time.Time{}
	}
	return !now.Before(s.Next)
}

func runMihomoOptimizer() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var schedule mihomoOptimizationSchedule
	for range ticker.C {
		c := rtCfg()
		if !schedule.due(c, time.Now()) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(15+c.MihomoMaxSwitches*c.MihomoProbeTimeoutSec)*time.Second)
		performMihomoOptimization(ctx, c)
		cancel()
		schedule.Next = time.Now().Add(time.Duration(c.MihomoOptimizeIntervalMin) * time.Minute)
		mihomoOptimizerState.Lock()
		mihomoOptimizerState.NextRun = schedule.Next.Unix()
		mihomoOptimizerState.Unlock()
	}
}

func handleAdminMihomoOptimize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c := rtCfg()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(15+c.MihomoMaxSwitches*c.MihomoProbeTimeoutSec)*time.Second)
	defer cancel()
	writeJSON(w, 200, performMihomoOptimization(ctx, c))
}

var mihomoConfigKeys = []string{"mihomo_auto_switch", "mihomo_controller", "mihomo_group", "mihomo_proxy_url", "mihomo_nodes", "mihomo_max_switches", "mihomo_cooldown_sec", "mihomo_prefer_low_latency", "mihomo_auto_optimize", "mihomo_optimize_interval_min", "mihomo_probe_timeout_sec"}

func handleAdminMihomoConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if r.Method == http.MethodPut {
		var fields map[string]json.RawMessage
		if json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&fields) != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		for key := range fields {
			valid := false
			for _, allowed := range mihomoConfigKeys {
				if key == allowed {
					valid = true
					break
				}
			}
			if !valid {
				writeJSON(w, 400, map[string]string{"error": "unknown mihomo setting"})
				return
			}
		}
		data, _ := json.Marshal(fields)
		next := rtCfg()
		if json.Unmarshal(data, &next) != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid setting type"})
			return
		}
		if err := saveRuntimeConfig(next); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	c := rtCfg()
	data, _ := json.Marshal(c)
	var all map[string]any
	json.Unmarshal(data, &all)
	settings := map[string]any{}
	for _, key := range mihomoConfigKeys {
		settings[key] = all[key]
	}
	mihomoOptimizerState.Lock()
	history := append([]mihomoOptimizationResult{}, mihomoOptimizerState.History...)
	running, nextRun := mihomoOptimizerState.Running, mihomoOptimizerState.NextRun
	mihomoOptimizerState.Unlock()
	if !c.MihomoAutoOptimize {
		nextRun = 0
	}
	writeJSON(w, 200, map[string]any{"config": settings, "secret_set": mihomoSecret() != "", "secret_from_panel": mihomoPanelSecretSet(), "running": running, "next_run": nextRun, "history": history})
}

func handleAdminMihomoLatency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	c := rtCfg()
	if validateMihomoConfig(c) != nil {
		writeJSON(w, 400, map[string]string{"error": "save a valid controller, Selector group and proxy URL first"})
		return
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var pool struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	if err := mihomoRequestContext(r.Context(), client, c, mihomoSecret(), "GET", "/proxies", nil, &pool); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	group, ok := pool.Proxies[c.MihomoGroup]
	if !ok || group.Type != "Selector" {
		writeJSON(w, 400, map[string]string{"error": "group must be a Selector"})
		return
	}
	var delays map[string]int
	path := "/group/" + url.PathEscape(c.MihomoGroup) + "/delay?url=" + url.QueryEscape("https://www.gstatic.com/generate_204") + "&timeout=5000"
	if err := mihomoRequestContext(r.Context(), client, c, mihomoSecret(), "GET", path, nil, &delays); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	type nodeDelay struct {
		Name    string `json:"name"`
		DelayMS int    `json:"delay_ms"`
		Current bool   `json:"current"`
	}
	nodes := []nodeDelay{}
	for _, n := range group.All {
		if mihomoLeaf(pool.Proxies[n]) {
			nodes = append(nodes, nodeDelay{n, delays[n], n == group.Now})
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool { a, b := nodes[i].DelayMS, nodes[j].DelayMS; return a > 0 && (b <= 0 || a < b) })
	writeJSON(w, 200, map[string]any{"nodes": nodes, "current_node": group.Now})
}
