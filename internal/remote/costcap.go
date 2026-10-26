package remote

import (
	"fmt"
	"math"
	"os"
	"strconv"
)

// CheckCostCap enforces HRN_REMOTE_MAX_COST_USD because tenant budgets are not reserved server-side.
func CheckCostCap(cost float64) error {
	raw := os.Getenv("HRN_REMOTE_MAX_COST_USD")
	if raw == "" {
		return nil
	}
	limit, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 {
		return fmt.Errorf("HRN_REMOTE_MAX_COST_USD must be a positive number of USD, got %q", raw)
	}
	if cost > limit {
		return fmt.Errorf("max cost %g USD exceeds HRN_REMOTE_MAX_COST_USD (%g USD)", cost, limit)
	}
	return nil
}
