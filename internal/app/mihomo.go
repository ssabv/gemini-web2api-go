package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

var errGeminiRegion = errors.New("upstream Gemini region is unsupported")

// Only inspect service error messages, never prompts or generated replies.
func isGeminiRegionMessage(s string) bool {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = strings.ReplaceAll(s, "’", "'")
	for _, marker := range []string{
		"user location is not supported for the api use",
		"gemini isn't currently supported in your country",
		"gemini is not currently supported in your country",
		"gemini is not supported in your country",
		"gemini isn't available in your country",
		"gemini is not available in your country",
		"gemini isn't available in your region",
		"gemini is not available in your region",
		"gemini 目前不支持你所在的国家",
		"gemini 目前不支持您所在的国家",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func isGeminiRegionResponse(status int, raw []byte, err error) bool {
	if errors.Is(err, errGeminiRegion) {
		return true
	}
	var wcErr *wcStatusError
	if errors.As(err, &wcErr) && isGeminiRegionMessage(wcErr.Msg) {
		return true
	}
	// A generated explanation of regional availability is not a service error.
	if hasContentFrame(string(raw)) {
		return false
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && isGeminiRegionMessage(envelope.Error.Message) {
		return true
	}
	// Legacy StreamGenerate can report region error 1060 inside the status details
	// of an otherwise empty HTTP 200 response. Inspect only typed service errors.
	for _, line := range strings.Split(string(raw), "\n") {
		var frames [][]json.RawMessage
		if !strings.Contains(line, `"wrb.fr"`) || json.Unmarshal([]byte(line), &frames) != nil {
			continue
		}
		for _, frame := range frames {
			var tag string
			var serviceError []json.RawMessage
			if len(frame) < 6 || json.Unmarshal(frame[0], &tag) != nil || tag != "wrb.fr" ||
				json.Unmarshal(frame[5], &serviceError) != nil || len(serviceError) < 3 {
				continue
			}
			var details [][]json.RawMessage
			if json.Unmarshal(serviceError[2], &details) != nil {
				continue
			}
			for _, detail := range details {
				var detailType string
				var info []json.RawMessage
				if len(detail) < 2 || json.Unmarshal(detail[0], &detailType) != nil ||
					detailType != "type.googleapis.com/assistant.boq.bard.application.BardErrorInfo" ||
					json.Unmarshal(detail[1], &info) != nil || len(info) == 0 {
					continue
				}
				var code int
				if json.Unmarshal(info[0], &code) == nil && code == 1060 {
					return true
				}
				var message string
				if len(info) > 2 && json.Unmarshal(info[2], &message) == nil && isGeminiRegionMessage(message) {
					return true
				}
			}
		}
	}
	// WebChannel status errors are returned as this exact plain-text prefix.
	return status >= 400 && strings.HasPrefix(string(raw), "processSession status ") &&
		isGeminiRegionMessage(string(raw))
}

func isGeminiRegionPage(body []byte) bool {
	// A working /app includes API/bootstrap credentials. Translation strings in its
	// scripts or hidden error templates must not make a supported page look blocked.
	if strings.Contains(string(body), `"d2zJAe"`) || snlm0eRe.Match(body) {
		return false
	}
	z := html.NewTokenizer(strings.NewReader(string(body)))
	var visible strings.Builder
	skip := ""
	for {
		switch z.Next() {
		case html.ErrorToken:
			return isGeminiRegionMessage(visible.String())
		case html.StartTagToken:
			tag, _ := z.TagName()
			if skip == "" && (string(tag) == "script" || string(tag) == "style" || string(tag) == "head" || string(tag) == "template") {
				skip = string(tag)
			}
		case html.EndTagToken:
			tag, _ := z.TagName()
			if string(tag) == skip {
				skip = ""
			}
		case html.TextToken:
			if skip == "" {
				visible.Write(z.Text())
				visible.WriteByte(' ')
			}
		}
	}
}

func validateMihomoController(controller string) error {
	u, err := url.Parse(controller)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("mihomo_controller must be an HTTP/HTTPS URL without credentials or query")
	}
	return nil
}

func validateMihomoConfig(c RuntimeConfig) error {
	if err := validateMihomoController(c.MihomoController); err != nil {
		return err
	}
	p, err := url.Parse(c.MihomoProxyURL)
	if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") ||
		p.User != nil || p.RawQuery != "" || p.Fragment != "" {
		return errors.New("mihomo_proxy_url must match a proxy pool URL without credentials or query")
	}
	if strings.TrimSpace(c.MihomoGroup) == "" || c.MihomoMaxSwitches < 1 || c.MihomoMaxSwitches > 5 {
		return errors.New("mihomo_group is required and mihomo_max_switches must be 1-5")
	}
	return nil
}

const mihomoSecretKey = "mihomo_controller_secret"

var (
	mihomoSecretMu    sync.RWMutex
	mihomoPanelSecret string
)

func initMihomoSecret() {
	mihomoSecretMu.Lock()
	mihomoPanelSecret = kvGet(mihomoSecretKey)
	mihomoSecretMu.Unlock()
}

func mihomoPanelSecretSet() bool {
	mihomoSecretMu.RLock()
	defer mihomoSecretMu.RUnlock()
	return mihomoPanelSecret != ""
}

func saveMihomoSecret(secret string) error {
	if len(secret) > 4096 || strings.ContainsAny(secret, "\r\n") {
		return errors.New("mihomo secret is too long")
	}
	mihomoSecretMu.Lock()
	defer mihomoSecretMu.Unlock()
	if err := kvSet(mihomoSecretKey, secret); err != nil {
		return err
	}
	mihomoPanelSecret = secret
	return nil
}

func mihomoSecret() string {
	mihomoSecretMu.RLock()
	secret := mihomoPanelSecret
	mihomoSecretMu.RUnlock()
	if secret != "" {
		return secret
	}
	if secret, ok := os.LookupEnv("MIHOMO_SECRET"); ok {
		return secret
	}
	return cfg.MihomoSecret
}

type mihomoProxy struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Now   string   `json:"now"`
	All   []string `json:"all"`
	Alive *bool    `json:"alive"`
}

type mihomoGroup struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Now        string `json:"now"`
	NodeCount  int    `json:"node_count"`
	Selectable bool   `json:"selectable"`
}

// Discovery only reads the controller and returns group metadata, never credentials
// or node configurations. It works before automatic switching is configured.
func listMihomoGroups(controller, secret string) ([]mihomoGroup, error) {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var pool struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	if err := mihomoRequest(client, RuntimeConfig{MihomoController: controller}, secret, "GET", "/proxies", nil, &pool); err != nil {
		return nil, err
	}
	groups := make([]mihomoGroup, 0)
	for name, proxy := range pool.Proxies {
		switch proxy.Type {
		case "Selector", "URLTest", "Fallback", "LoadBalance", "Relay":
			groups = append(groups, mihomoGroup{Name: name, Type: proxy.Type, Now: proxy.Now,
				NodeCount: len(proxy.All), Selectable: proxy.Type == "Selector"})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, nil
}

// Direct controller requests must not inherit the proxy they are controlling.
func mihomoRequest(client *http.Client, c RuntimeConfig, secret, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, strings.TrimRight(c.MihomoController, "/")+path, body)
	if err != nil {
		return errors.New("mihomo controller URL is invalid")
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("mihomo controller request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mihomo controller HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
			return errors.New("mihomo controller returned invalid JSON")
		}
	}
	return nil
}

type mihomoSwitcher struct {
	mu         sync.Mutex
	key        string
	lastSwitch time.Time
	lastNode   string
	blocked    map[string]time.Time
}

var mihomoSwitches = &mihomoSwitcher{}

func (s *mihomoSwitcher) switchNode(c RuntimeConfig, secret string, started time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := c.MihomoController + "\n" + c.MihomoGroup
	if s.key != key {
		s.key, s.lastSwitch, s.lastNode, s.blocked = key, time.Time{}, "", map[string]time.Time{}
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var pool struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	if err := mihomoRequest(client, c, secret, "GET", "/proxies", nil, &pool); err != nil {
		return err
	}
	group, ok := pool.Proxies[c.MihomoGroup]
	if !ok || group.Type != "Selector" || group.Now == "" {
		return errors.New("mihomo group must be a nonempty Selector")
	}
	// Failures from a request that began on the old node share the completed switch.
	if s.lastSwitch.After(started) && group.Now == s.lastNode {
		return nil
	}
	now := time.Now()
	s.blocked[group.Now] = now.Add(30 * time.Minute)
	for name, until := range s.blocked {
		if !until.After(now) {
			delete(s.blocked, name)
		}
	}
	if now.Sub(s.lastSwitch) < time.Duration(c.MihomoCooldownSec)*time.Second {
		return errors.New("mihomo switch cooldown is active")
	}
	names := group.All
	if strings.TrimSpace(c.MihomoNodes) != "" {
		names = strings.FieldsFunc(c.MihomoNodes, func(r rune) bool { return r == '\n' || r == '\r' || r == '|' })
	}
	members := map[string]bool{}
	for _, name := range group.All {
		members[name] = true
	}
	start := 0
	for i, name := range names {
		if strings.TrimSpace(name) == group.Now {
			start = i + 1
			break
		}
	}
	for i := 0; i < len(names); i++ {
		name := strings.TrimSpace(names[(start+i)%len(names)])
		p, exists := pool.Proxies[name]
		if !exists || p.Type == "" || !members[name] || name == group.Now || s.blocked[name].After(now) ||
			len(p.All) > 0 || p.Type == "Selector" || p.Type == "URLTest" || p.Type == "Fallback" || p.Type == "LoadBalance" || p.Type == "Relay" ||
			strings.EqualFold(p.Type, "direct") || strings.EqualFold(p.Type, "reject") || strings.EqualFold(p.Type, "rejectdrop") ||
			strings.EqualFold(p.Type, "pass") || strings.EqualFold(p.Type, "dns") || strings.EqualFold(p.Type, "compatible") ||
			(p.Alive != nil && !*p.Alive) {
			continue
		}
		body, _ := json.Marshal(map[string]string{"name": name})
		if err := mihomoRequest(client, c, secret, "PUT", "/proxies/"+url.PathEscape(c.MihomoGroup), strings.NewReader(string(body)), nil); err != nil {
			return err
		}
		var selected mihomoProxy
		if err := mihomoRequest(client, c, secret, "GET", "/proxies/"+url.PathEscape(c.MihomoGroup), nil, &selected); err != nil {
			return err
		}
		if selected.Now != name {
			return errors.New("mihomo controller did not confirm node selection")
		}
		s.lastSwitch, s.lastNode = time.Now(), name
		// Never delete /connections: only this process's idle proxy tunnels need renewal.
		stdlibClientCache.Range(func(k, v any) bool {
			if strings.HasPrefix(k.(string), c.MihomoProxyURL+"|") {
				v.(*http.Client).CloseIdleConnections()
			}
			return true
		})
		logf("[mihomo] region unsupported: group=%q node=%q -> %q", c.MihomoGroup, group.Now, name)
		return nil
	}
	return errors.New("mihomo group has no eligible unblocked node")
}

func generateWithMihomoRecovery(generate func() (*StreamResult, error)) (*StreamResult, error) {
	c := rtCfg()
	secret := mihomoSecret()
	for switches := 0; ; switches++ {
		started := time.Now()
		res, err := generate()
		if !c.MihomoAutoSwitch || !errors.Is(err, errGeminiRegion) || switches >= c.MihomoMaxSwitches ||
			res == nil || res.ProxyID == 0 || res.Emitted != "" || res.EmittedReasoning != "" {
			return res, err
		}
		if validateMihomoConfig(c) != nil {
			return res, err
		}
		proxyMu.RLock()
		matched := false
		for _, p := range proxyCache {
			if p.ID == res.ProxyID && p.URL == c.MihomoProxyURL {
				matched = true
				break
			}
		}
		proxyMu.RUnlock()
		if !matched {
			return res, err
		}
		if switchErr := mihomoSwitches.switchNode(c, secret, started); switchErr != nil {
			logf("[mihomo] region recovery stopped: %v", switchErr)
			return res, err
		}
	}
}
