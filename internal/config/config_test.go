package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestLoadExampleShape(t *testing.T) {
	yaml := `
listen: "0.0.0.0:9999"
upstream: "https://api.example.com"
tier: aggressive
targets: {balanced: 0.55, aggressive: 0.92}
exempt:
  system: false
  last_user: true
cache:
  enabled: false
  capacity: 64
diff_mode: false
latency_budget_ms: 15
rate:
  default_rps: 12.5
tokenest:
  ascii_chars_per_token: 3.8
  wide_chars_per_token: 1.2
json_pruner:
  long_string_cap: 500
  keyword_sample: 3
high_value_keys: [id, sha]
pricing:
  "gpt-4o": {input: 2.50, output: 10.00}
stats_jsonl: "/tmp/tokencut.jsonl"
log_level: debug
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9999" || cfg.Tier != "aggressive" {
		t.Errorf("scalars: %+v", cfg)
	}
	if cfg.Targets["aggressive"] != 0.92 || cfg.Targets["balanced"] != 0.55 {
		t.Errorf("targets: %v", cfg.Targets)
	}
	if cfg.ExemptSystem || !cfg.ExemptLastUser {
		t.Errorf("exempt: %+v", cfg)
	}
	if cfg.CacheEnabled || cfg.CacheCapacity != 64 {
		t.Errorf("cache: %+v", cfg)
	}
	if cfg.RateRPS != 12.5 {
		t.Errorf("rate: %v", cfg.RateRPS)
	}
	if cfg.TokenKindConstants["text"] != 3.8 || cfg.LongStringCap != 500 || cfg.KeywordSample != 3 {
		t.Errorf("tokenest: %+v", cfg)
	}
	if len(cfg.HighValueKeys) != 2 || cfg.HighValueKeys[0] != "id" {
		t.Errorf("high_value_keys: %v", cfg.HighValueKeys)
	}
	if p := cfg.Pricing["gpt-4o"]; p.Input != 2.5 || p.Output != 10.0 {
		t.Errorf("pricing: %v", cfg.Pricing)
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg := Default()
	t.Setenv("TOKENCUT_LISTEN", "127.0.0.1:1234")
	t.Setenv("TOKENCUT_UPSTREAM", "http://localhost:9")
	t.Setenv("TOKENCUT_TIER", "aggressive")
	cfg.ApplyEnv()
	if cfg.Listen != "127.0.0.1:1234" || cfg.Upstream != "http://localhost:9" || cfg.Tier != "aggressive" {
		t.Errorf("env override failed: %+v", cfg)
	}
}

func TestValidateRejectsBadTier(t *testing.T) {
	cfg := Default()
	cfg.Tier = "turbo"
	if err := cfg.validate(); err == nil {
		t.Errorf("bad tier accepted")
	}
	cfg.Tier = "balanced"
	cfg.Targets["balanced"] = 1.5
	if err := cfg.validate(); err == nil {
		t.Errorf("out-of-range target accepted")
	}
}
