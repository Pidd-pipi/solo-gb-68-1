package services

import "testing"

func TestNormalizeEstimatedUsage(t *testing.T) {
	tests := []struct {
		name     string
		estimate float64
		want     float64
	}{
		{name: "未提供（0）按默认值兜底", estimate: 0, want: DefaultEstimatedWaterUsage},
		{name: "负数按默认值兜底", estimate: -5, want: DefaultEstimatedWaterUsage},
		{name: "极小正数按默认值兜底", estimate: 0.0001, want: DefaultEstimatedWaterUsage},
		{name: "小于默认值的预估按默认值兜底", estimate: 5, want: DefaultEstimatedWaterUsage},
		{name: "等于默认值保持不变", estimate: DefaultEstimatedWaterUsage, want: DefaultEstimatedWaterUsage},
		{name: "大于默认值保持不变", estimate: 30, want: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeEstimatedUsage(tt.estimate); got != tt.want {
				t.Errorf("normalizeEstimatedUsage(%v) = %v, want %v", tt.estimate, got, tt.want)
			}
		})
	}
}
