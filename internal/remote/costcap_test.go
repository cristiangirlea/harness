package remote

import (
	"strings"
	"testing"
)

func TestCheckCostCapAllowsAnyCeilingWhenUnset(t *testing.T) {
	t.Setenv("HRN_REMOTE_MAX_COST_USD", "")
	if err := CheckCostCap(1000); err != nil {
		t.Fatalf("unset cap refused: %v", err)
	}
}

func TestCheckCostCapRefusesCeilingsAboveTheLimit(t *testing.T) {
	t.Setenv("HRN_REMOTE_MAX_COST_USD", "2.5")
	for _, cost := range []float64{0.25, 2.5} {
		if err := CheckCostCap(cost); err != nil {
			t.Errorf("cost %v at or under the cap refused: %v", cost, err)
		}
	}
	err := CheckCostCap(2.51)
	if err == nil || !strings.Contains(err.Error(), "HRN_REMOTE_MAX_COST_USD") || !strings.Contains(err.Error(), "2.5") {
		t.Fatalf("cost above the cap: err = %v, want the limit named", err)
	}
}

func TestCheckCostCapRefusesAnInvalidLimit(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-1", "NaN", "+Inf"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("HRN_REMOTE_MAX_COST_USD", raw)
			err := CheckCostCap(0.01)
			if err == nil || !strings.Contains(err.Error(), "HRN_REMOTE_MAX_COST_USD") {
				t.Fatalf("invalid cap %q: err = %v, want the variable named", raw, err)
			}
		})
	}
}
