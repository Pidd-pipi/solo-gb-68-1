package services

import (
	"testing"
	"time"
)

func TestEvaluateBudget(t *testing.T) {
	tests := []struct {
		name             string
		used             float64
		reserved         float64
		estimate         float64
		limit            float64
		thresholdPct     float64
		wantAllowed      bool
		wantThresholdHit bool
	}{
		{name: "无用量时允许且不告警", used: 0, reserved: 0, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: true, wantThresholdHit: false},
		{name: "低于阈值允许", used: 50, reserved: 0, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: true, wantThresholdHit: false},
		{name: "恰好达到阈值时告警", used: 80, reserved: 0, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: true, wantThresholdHit: true},
		{name: "预估用量计入后达到阈值", used: 70, reserved: 0, estimate: 15, limit: 100, thresholdPct: 80, wantAllowed: true, wantThresholdHit: true},
		{name: "达到上限时拦截", used: 100, reserved: 0, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: false, wantThresholdHit: true},
		{name: "超过上限时拦截", used: 120, reserved: 0, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: false, wantThresholdHit: true},
		{name: "已用加预留达到上限时拦截（并发场景）", used: 90, reserved: 10, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: false, wantThresholdHit: true},
		{name: "预估用量将超上限时拦截", used: 95, reserved: 0, estimate: 10, limit: 100, thresholdPct: 80, wantAllowed: false, wantThresholdHit: true},
		{name: "预估后恰好等于上限时允许", used: 90, reserved: 0, estimate: 10, limit: 100, thresholdPct: 80, wantAllowed: true, wantThresholdHit: true},
		{name: "预留占用计入阈值判定", used: 70, reserved: 15, estimate: 0, limit: 100, thresholdPct: 80, wantAllowed: true, wantThresholdHit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, thresholdHit, reason := evaluateBudget(tt.used, tt.reserved, tt.estimate, tt.limit, tt.thresholdPct)
			if allowed != tt.wantAllowed {
				t.Errorf("allowed = %v, want %v (reason: %s)", allowed, tt.wantAllowed, reason)
			}
			if thresholdHit != tt.wantThresholdHit {
				t.Errorf("thresholdReached = %v, want %v", thresholdHit, tt.wantThresholdHit)
			}
			if !allowed && reason == "" {
				t.Error("拦截时应返回原因说明")
			}
		})
	}
}

func TestComputeUsage(t *testing.T) {
	tests := []struct {
		name          string
		limit         float64
		used          float64
		reserved      float64
		wantRemaining float64
		wantPercent   float64
	}{
		{name: "无用量", limit: 100, used: 0, reserved: 0, wantRemaining: 100, wantPercent: 0},
		{name: "部分使用", limit: 100, used: 30, reserved: 0, wantRemaining: 70, wantPercent: 30},
		{name: "预留计入剩余量", limit: 100, used: 30, reserved: 20, wantRemaining: 50, wantPercent: 50},
		{name: "超额后剩余为零", limit: 100, used: 120, reserved: 0, wantRemaining: 0, wantPercent: 120},
		{name: "零上限不产生除零", limit: 0, used: 0, reserved: 0, wantRemaining: 0, wantPercent: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remaining, percent := computeUsage(tt.limit, tt.used, tt.reserved)
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %v, want %v", remaining, tt.wantRemaining)
			}
			if percent != tt.wantPercent {
				t.Errorf("percent = %v, want %v", percent, tt.wantPercent)
			}
		})
	}
}

func TestCurrentPeriodAndMonthStart(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 30, 0, 0, time.Local)

	if got := currentPeriod(now); got != "2026-09" {
		t.Errorf("currentPeriod = %s, want 2026-09", got)
	}

	start := monthStart(now)
	if start.Year() != 2026 || start.Month() != 9 || start.Day() != 1 ||
		start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 {
		t.Errorf("monthStart = %v, want 2026-09-01 00:00:00", start)
	}
}
