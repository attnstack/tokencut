// config.go — configuration loading: YAML-subset file + env overrides +
// defaults, with validation. Every knob has a documented default so a bare
// `tokencut` (no config file) is a working proxy.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"tokencut/internal/yamlite"
)

type PricingEntry struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type Config struct {
	Listen          string
	Upstream        string
	Tier            string
	Targets         map[string]float64
	ExemptSystem    bool
	ExemptLastUser  bool
	CacheEnabled    bool
	CacheCapacity   int
	DiffMode        bool
	LatencyBudgetMS int
	MaxBodyBytes    int
	RateRPS         float64
	// TokenKindConstants maps classify kind → ASCII chars per token; fitted
	// by bench/calibrate.py (see methodology). TokenWidePerToken prices
	// non-ASCII runes for every kind.
	TokenKindConstants map[string]float64
	TokenWidePerToken  float64
	LongStringCap      int
	KeywordSample      int
	HighValueKeys      []string
	Pricing            map[string]PricingEntry
	StatsJSONL         string
	LogLevel           string
}

func Default() *Config {
	return &Config{
		Listen:          "127.0.0.1:8787",
		Upstream:        "https://api.openai.com",
		Tier:            "balanced",
		Targets:         map[string]float64{"balanced": 0.60, "aggressive": 0.90, "lossless": 0},
		ExemptSystem:    true,
		ExemptLastUser:  true,
		CacheEnabled:    true,
		CacheCapacity:   1024,
		DiffMode:        true,
		LatencyBudgetMS: 30,
		MaxBodyBytes:    32 << 20,
		RateRPS:         0,
		TokenKindConstants: map[string]float64{
			// cl100k_base fits on the benchmark corpus (bench/calibrate.py);
			// o200k_base ASCII constants agree within ~1%.
			"json": 3.12,
			"log":  2.34,
			"code": 3.80,
			"text": 4.46,
		},
		TokenWidePerToken: 0.74,
		LongStringCap:     2000,
		KeywordSample:     2,
		HighValueKeys: []string{
			"id", "name", "type", "status", "error", "code", "message", "url", "data", "key", "path", "file",
		},
		Pricing:  map[string]PricingEntry{},
		LogLevel: "info",
	}
}

// Load reads a YAML-subset config on top of the defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	raw, err := yamlite.Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := cfg.applyMap(raw); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ApplyEnv applies TOKENCUT_* env overrides (12-factor; also how the
// benchmark harness and CI point the proxy around without editing files).
func (c *Config) ApplyEnv() {
	if v := os.Getenv("TOKENCUT_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TOKENCUT_UPSTREAM"); v != "" {
		c.Upstream = v
	}
	if v := os.Getenv("TOKENCUT_TIER"); v != "" {
		c.Tier = v
	}
	if v := os.Getenv("TOKENCUT_STATS_JSONL"); v != "" {
		c.StatsJSONL = v
	}
}

func (c *Config) validate() error {
	switch c.Tier {
	case "lossless", "balanced", "aggressive":
	default:
		return fmt.Errorf("config: unknown tier %q (want lossless|balanced|aggressive)", c.Tier)
	}
	for name, r := range c.Targets {
		if r < 0 || r >= 1 {
			return fmt.Errorf("config: target %q = %v out of range [0,1)", name, r)
		}
	}
	if c.LatencyBudgetMS <= 0 || c.MaxBodyBytes <= 0 || c.CacheCapacity <= 0 {
		return fmt.Errorf("config: latency_budget_ms / max_body_bytes / cache_capacity must be positive")
	}
	if c.Upstream == "" || !strings.HasPrefix(c.Upstream, "http") {
		return fmt.Errorf("config: upstream must be an http(s) URL, got %q", c.Upstream)
	}
	return nil
}

func (c *Config) applyMap(raw map[string]any) error {
	str := func(key, def string) string {
		if v, ok := raw[key].(string); ok && v != "" {
			return v
		}
		return def
	}
	fl := func(key string, def float64) float64 {
		switch v := raw[key].(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		}
		return def
	}
	bl := func(key string, def bool) bool {
		if v, ok := raw[key].(bool); ok {
			return v
		}
		return def
	}
	in := func(key string, def int) int {
		switch v := raw[key].(type) {
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
		return def
	}

	c.Listen = str("listen", c.Listen)
	c.Upstream = str("upstream", c.Upstream)
	c.Tier = str("tier", c.Tier)
	c.DiffMode = bl("diff_mode", c.DiffMode)
	c.LatencyBudgetMS = in("latency_budget_ms", c.LatencyBudgetMS)
	c.MaxBodyBytes = in("max_body_bytes", c.MaxBodyBytes)
	c.RateRPS = fl("rate_rps", c.RateRPS)
	c.StatsJSONL = str("stats_jsonl", c.StatsJSONL)
	c.LogLevel = str("log_level", c.LogLevel)

	if sec, ok := raw["targets"].(map[string]any); ok {
		for _, name := range []string{"lossless", "balanced", "aggressive"} {
			if v, ok := sec[name]; ok {
				switch t := v.(type) {
				case float64:
					c.Targets[name] = t
				case int64:
					c.Targets[name] = float64(t)
				}
			}
		}
	}
	if sec, ok := raw["exempt"].(map[string]any); ok {
		if v, ok := sec["system"].(bool); ok {
			c.ExemptSystem = v
		}
		if v, ok := sec["last_user"].(bool); ok {
			c.ExemptLastUser = v
		}
	}
	if sec, ok := raw["cache"].(map[string]any); ok {
		if v, ok := sec["enabled"].(bool); ok {
			c.CacheEnabled = v
		}
		if v, ok := sec["capacity"]; ok {
			switch t := v.(type) {
			case int64:
				c.CacheCapacity = int(t)
			case float64:
				c.CacheCapacity = int(t)
			}
		}
	}
	if sec, ok := raw["tokenest"].(map[string]any); ok {
		// Per-kind constants (chars per token), fitted by bench/calibrate.py.
		// Keys: {json,log,code,text,table,html}_chars_per_token plus
		// wide_chars_per_token (applies to every kind). The legacy flat keys
		// (ascii_chars_per_token) are still accepted and price the text kind.
		getFloat := func(key string) (float64, bool) {
			v, ok := sec[key]
			if !ok {
				return 0, false
			}
			switch t := v.(type) {
			case float64:
				return t, true
			case int64:
				return float64(t), true
			}
			return 0, false
		}
		for _, kind := range []string{"json", "log", "code", "text", "table", "html"} {
			if v, ok := getFloat(kind + "_chars_per_token"); ok && v > 0 {
				if c.TokenKindConstants == nil {
					c.TokenKindConstants = map[string]float64{}
				}
				c.TokenKindConstants[kind] = v
			}
		}
		if v, ok := getFloat("wide_chars_per_token"); ok && v > 0 {
			c.TokenWidePerToken = v
		}
		if v, ok := getFloat("ascii_chars_per_token"); ok && v > 0 {
			if c.TokenKindConstants == nil {
				c.TokenKindConstants = map[string]float64{}
			}
			c.TokenKindConstants["text"] = v
		}
	}
	if sec, ok := raw["json_pruner"].(map[string]any); ok {
		if v, ok := sec["long_string_cap"]; ok {
			switch t := v.(type) {
			case int64:
				c.LongStringCap = int(t)
			case float64:
				c.LongStringCap = int(t)
			}
		}
		if v, ok := sec["keyword_sample"]; ok {
			switch t := v.(type) {
			case int64:
				c.KeywordSample = int(t)
			case float64:
				c.KeywordSample = int(t)
			}
		}
	}
	if sec, ok := raw["rate"].(map[string]any); ok {
		if v, ok := sec["default_rps"]; ok {
			switch t := v.(type) {
			case float64:
				c.RateRPS = t
			case int64:
				c.RateRPS = float64(t)
			}
		}
	}
	if v, ok := raw["high_value_keys"].([]any); ok {
		keys := make([]string, 0, len(v))
		for _, k := range v {
			if s, ok := k.(string); ok && s != "" {
				keys = append(keys, s)
			}
		}
		if len(keys) > 0 {
			c.HighValueKeys = keys
		}
	}
	if sec, ok := raw["pricing"].(map[string]any); ok {
		b, err := json.Marshal(sec)
		if err != nil {
			return fmt.Errorf("pricing section: %w", err)
		}
		pricing := map[string]PricingEntry{}
		if err := json.Unmarshal(b, &pricing); err != nil {
			return fmt.Errorf("pricing section: %w", err)
		}
		c.Pricing = pricing
	}
	return nil
}
