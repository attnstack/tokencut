package tokenest

import "testing"

func TestEstimateForKindDifferences(t *testing.T) {
	// The whole point of per-kind constants: JSON (dense punctuation) prices
	// HIGHER than prose at the same length.
	jsonS := `{"id": 7, "name": "item-7", "status": "active", "payload": "xxxx"}`
	if EstimateFor("json", jsonS) <= EstimateFor("text", jsonS) {
		t.Errorf("json pricing must exceed text for dense JSON: %d vs %d",
			EstimateFor("json", jsonS), EstimateFor("text", jsonS))
	}
}

func TestEstimateEmptyAndMinimum(t *testing.T) {
	if EstimateFor("text", "") != 0 {
		t.Errorf("empty = %d, want 0", EstimateFor("text", ""))
	}
	if EstimateFor("text", "a") != 1 {
		t.Errorf("single char = %d, want 1", EstimateFor("text", "a"))
	}
}

func TestWideRunesPricedHigher(t *testing.T) {
	wide := "中文内容测试"
	ascii := "abcdefghij"
	if EstimateFor("text", wide) <= EstimateFor("text", ascii) {
		t.Errorf("wide runes (%d) must cost more than same-count ASCII (%d)",
			EstimateFor("text", wide), EstimateFor("text", ascii))
	}
}

func TestConfigureKindOverride(t *testing.T) {
	before := EstimateFor("json", `{"a":"b"}`)
	ConfigureKind("json", 1.0, 0) // every ASCII rune = one token
	after := EstimateFor("json", `{"a":"b"}`)
	if after <= before {
		t.Errorf("override must raise the estimate: %d -> %d", before, after)
	}
	ConfigureKind("json", 3.12, 0.74) // restore calibrated default
}

func TestRunesForTokens(t *testing.T) {
	if RunesForTokens(0) != 0 || RunesForTokens(-1) != 0 {
		t.Errorf("non-positive budgets must map to 0")
	}
	if RunesForTokens(10) != int(10*constsFor("json").ascii) {
		t.Errorf("RunesForTokens must price at the json constants")
	}
}
