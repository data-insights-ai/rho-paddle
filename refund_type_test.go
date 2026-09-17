package paddle

import "testing"

// A transaction-level full adjustment omits the items array, which makes Paddle
// refund the whole transaction. It must therefore be reachable only when the
// frozen plan covers every bound provider line in full.
func TestRefundTypeOnlyEscalatesToFullForAWholePlan(t *testing.T) {
	subset := RefundDispatch{
		Whole: false,
		Items: []RefundDispatchItem{{ProviderLineID: "txnitm_a", Type: AdjustmentFull}},
	}
	if got := refundType(subset); got != AdjustmentPartial {
		t.Fatalf("subset of a multi-line transaction sent as %q, want partial", got)
	}
	whole := subset
	whole.Whole = true
	if got := refundType(whole); got != AdjustmentFull {
		t.Fatalf("whole plan sent as %q, want full", got)
	}
	mixed := RefundDispatch{
		Whole: true,
		Items: []RefundDispatchItem{
			{ProviderLineID: "txnitm_a", Type: AdjustmentFull},
			{ProviderLineID: "txnitm_b", Type: AdjustmentPartial, Amount: "10"},
		},
	}
	if got := refundType(mixed); got != AdjustmentPartial {
		t.Fatalf("plan with a partial item sent as %q, want partial", got)
	}
}
