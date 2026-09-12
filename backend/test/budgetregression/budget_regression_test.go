// Package budgetregression 是用水预算的端到端回归测试套件，
// 覆盖手动灌溉与自动调度两类触发路径：
//   - 极小正数预估用水按最小额度（DefaultEstimatedWaterUsage）预留；
//   - 超限时拦截且不新增灌溉记录；
//   - 阈值告警与超限告警本月内只生成一次；
//   - 预算停用/启用后的管控切换；
//   - 并发创建预算仅一次成功；
//   - 接口层确认超限时返回 409 Conflict。
//
// 每个用例前重置测试库 schema（testutil.SetupPostgresDB），可重复运行结果一致。
// 运行：cd backend && go test ./test/budgetregression/ -v
package budgetregression

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"irrigation/internal/config"
	"irrigation/internal/middleware"
	"irrigation/internal/models"
	"irrigation/internal/routes"
	"irrigation/internal/scheduler"
	"irrigation/internal/services"
	"irrigation/internal/testutil"
	"irrigation/pkg/database"
	"irrigation/pkg/logger"
)

// ---------- 测试环境 ----------

var setupOnce sync.Once

// setup 准备回归测试环境：测试数据库（重置 schema）、日志、JWT 配置与完整路由。
// 返回带全部接口的 gin 引擎与可用的认证 token。
func setup(t *testing.T) (*gin.Engine, string) {
	t.Helper()

	setupOnce.Do(func() {
		logger.Init("error")
		config.AppSettings = &config.Config{
			JWT: config.JWTConfig{Secret: "budget-regression-secret", ExpireHours: 1},
		}
		gin.SetMode(gin.TestMode)
	})
	testutil.SetupPostgresDB(t)

	router := gin.New()
	routes.SetupRoutes(router)

	token, err := middleware.GenerateToken(1, "regression-tester")
	if err != nil {
		t.Fatalf("生成测试 token 失败: %v", err)
	}
	return router, token
}

// ---------- 数据构造与断言辅助 ----------

func mustCreateZone(t *testing.T, name string) models.IrrigationZone {
	t.Helper()
	zone := models.IrrigationZone{Name: name}
	if err := database.DB.Create(&zone).Error; err != nil {
		t.Fatalf("创建区域失败: %v", err)
	}
	return zone
}

func mustCreateBudget(t *testing.T, zoneID uint, limit, threshold float64) models.WaterBudget {
	t.Helper()
	budget := &models.WaterBudget{
		ZoneID:         zoneID,
		MonthlyLimit:   limit,
		AlertThreshold: threshold,
	}
	if err := services.NewBudgetService().CreateBudget(budget); err != nil {
		t.Fatalf("创建预算失败: %v", err)
	}
	return *budget
}

func mustCreateSchedule(t *testing.T, name string, scheduleType models.ScheduleType, zoneID uint, duration int) models.IrrigationSchedule {
	t.Helper()
	s := models.IrrigationSchedule{
		Name:       name,
		Type:       scheduleType,
		ZoneID:     &zoneID,
		Status:     models.ScheduleStatusActive,
		StartTime:  "06:00",
		Duration:   duration,
		RepeatMode: models.RepeatModeDaily,
	}
	if err := database.DB.Create(&s).Error; err != nil {
		t.Fatalf("创建灌溉计划失败: %v", err)
	}
	return s
}

func countIrrigationLogs(t *testing.T, zoneID uint) int64 {
	t.Helper()
	var count int64
	if err := database.DB.Model(&models.IrrigationLog{}).Where("zone_id = ?", zoneID).Count(&count).Error; err != nil {
		t.Fatalf("统计灌溉记录失败: %v", err)
	}
	return count
}

func countAlerts(t *testing.T, zoneID uint, alertType models.AlertType) int64 {
	t.Helper()
	var count int64
	if err := database.DB.Model(&models.Alert{}).
		Where("zone_id = ? AND type = ?", zoneID, alertType).
		Count(&count).Error; err != nil {
		t.Fatalf("统计告警失败: %v", err)
	}
	return count
}

func budgetUsage(t *testing.T, budgetID uint) *services.BudgetUsage {
	t.Helper()
	usage, err := services.NewBudgetService().GetBudgetUsage(budgetID)
	if err != nil {
		t.Fatalf("查询预算用量失败: %v", err)
	}
	return usage
}

// ---------- 接口层请求辅助 ----------

type apiResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func doJSON(t *testing.T, router *gin.Engine, token, method, path string, body interface{}) (int, apiResponse) {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	var resp apiResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v, body: %s", err, recorder.Body.String())
	}
	return recorder.Code, resp
}

// ---------- 手动灌溉触发 ----------

// 手动灌溉传入极小正数（及 0、负数）预估用水时，一律按最小额度预留，预算保护不被绕过。
func TestManualTinyEstimateReservesMinimum(t *testing.T) {
	setup(t)
	irrigationSvc := services.NewIrrigationService()
	zone := mustCreateZone(t, "手动极小预估区")
	budget := mustCreateBudget(t, zone.ID, 100, 80)

	estimates := []float64{0.0001, 0, -3}
	for _, estimate := range estimates {
		if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, estimate); err != nil {
			t.Fatalf("预估 %v 触发失败: %v", estimate, err)
		}
	}

	// 每次触发都应按最小额度 DefaultEstimatedWaterUsage 预留
	var amounts []float64
	if err := database.DB.Model(&models.WaterBudgetReservation{}).
		Where("zone_id = ?", zone.ID).
		Order("id ASC").
		Pluck("amount", &amounts).Error; err != nil {
		t.Fatalf("查询预留记录失败: %v", err)
	}
	if len(amounts) != len(estimates) {
		t.Fatalf("预留记录数应为 %d，实际 %d", len(estimates), len(amounts))
	}
	for i, amount := range amounts {
		if amount != services.DefaultEstimatedWaterUsage {
			t.Errorf("第 %d 条预留额度应为 %.2f，实际 %.2f", i, services.DefaultEstimatedWaterUsage, amount)
		}
	}

	usage := budgetUsage(t, budget.ID)
	want := services.DefaultEstimatedWaterUsage * float64(len(estimates))
	if usage.ReservedAmount != want {
		t.Errorf("预留总额应为 %.2f，实际 %.2f", want, usage.ReservedAmount)
	}
}

// 手动灌溉超限时被拦截且不新增灌溉记录；超限告警本月只生成一次。
func TestManualExceededBlockedWithoutNewLog(t *testing.T) {
	setup(t)
	irrigationSvc := services.NewIrrigationService()
	zone := mustCreateZone(t, "手动超限拦截区")
	mustCreateBudget(t, zone.ID, 20, 80)

	// 第一次：预留 15，通过
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 15); err != nil {
		t.Fatalf("首次触发不应被拦截: %v", err)
	}

	// 第二次：15+15=30 超过上限 20，必须拦截
	before := countIrrigationLogs(t, zone.ID)
	_, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 15)
	if !errors.Is(err, services.ErrBudgetExceeded) {
		t.Fatalf("超限应返回 ErrBudgetExceeded，实际 %v", err)
	}
	if after := countIrrigationLogs(t, zone.ID); after != before {
		t.Errorf("被拦截时不应新增灌溉记录，拦截前 %d 条，拦截后 %d 条", before, after)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetExceeded); got != 1 {
		t.Errorf("超限告警应生成 1 条，实际 %d", got)
	}

	// 第三次：仍超限，告警本月不重复生成
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 15); !errors.Is(err, services.ErrBudgetExceeded) {
		t.Fatalf("持续超限应继续拦截，实际 %v", err)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetExceeded); got != 1 {
		t.Errorf("超限告警本月只应生成一次，实际 %d 条", got)
	}
	if got := countIrrigationLogs(t, zone.ID); got != before {
		t.Errorf("拦截期间灌溉记录不应增加，实际 %d 条", got)
	}
}

// ---------- 自动调度触发 ----------

// 定时计划 duration=1 时预估用水仅 0.1（极小正数），触发时应按最小额度预留，
// 执行完成后按实际用水结算。
func TestScheduledTinyEstimateReservesMinimum(t *testing.T) {
	setup(t)
	zone := mustCreateZone(t, "定时极小预估区")
	budget := mustCreateBudget(t, zone.ID, 100, 80)
	schedule := mustCreateSchedule(t, "每日滴灌", models.ScheduleTypeTimed, zone.ID, 1)

	scheduler.NewIrrigationScheduler().ExecuteIrrigation(schedule)

	// 灌溉记录：定时触发、执行成功、实际用水 0.1
	var log models.IrrigationLog
	if err := database.DB.Where("schedule_id = ?", schedule.ID).First(&log).Error; err != nil {
		t.Fatalf("定时调度应产生灌溉记录: %v", err)
	}
	if log.TriggerType != models.TriggerTypeTimed {
		t.Errorf("触发方式应为 timed，实际 %s", log.TriggerType)
	}
	if log.Status != models.ExecutionStatusSuccess {
		t.Errorf("执行状态应为 success，实际 %s", log.Status)
	}
	if log.WaterUsage == nil || *log.WaterUsage != 0.1 {
		t.Errorf("实际用水应为 0.1，实际 %v", log.WaterUsage)
	}

	// 预留记录：触发时按最小额度 10 预留，完成后已结算
	var reservation models.WaterBudgetReservation
	if err := database.DB.Where("log_id = ?", log.ID).First(&reservation).Error; err != nil {
		t.Fatalf("定时调度应创建预算预留: %v", err)
	}
	if reservation.Amount != services.DefaultEstimatedWaterUsage {
		t.Errorf("极小预估应按最小额度 %.2f 预留，实际 %.2f", services.DefaultEstimatedWaterUsage, reservation.Amount)
	}
	if reservation.Status != models.ReservationStatusSettled {
		t.Errorf("完成后预留应结算为 settled，实际 %s", reservation.Status)
	}

	usage := budgetUsage(t, budget.ID)
	if usage.UsedAmount != 0.1 || usage.ReservedAmount != 0 {
		t.Errorf("结算后已用应为 0.1、预留应为 0: %+v", usage)
	}
}

// 条件计划触发时灌溉记录应标记为 conditional，并同样占用预算额度。
func TestScheduledConditionalTrigger(t *testing.T) {
	setup(t)
	zone := mustCreateZone(t, "条件触发区")
	budget := mustCreateBudget(t, zone.ID, 100, 80)
	schedule := mustCreateSchedule(t, "低湿度触发", models.ScheduleTypeConditional, zone.ID, 1)

	scheduler.NewIrrigationScheduler().ExecuteIrrigation(schedule)

	var log models.IrrigationLog
	if err := database.DB.Where("schedule_id = ?", schedule.ID).First(&log).Error; err != nil {
		t.Fatalf("条件调度应产生灌溉记录: %v", err)
	}
	if log.TriggerType != models.TriggerTypeConditional {
		t.Errorf("触发方式应为 conditional，实际 %s", log.TriggerType)
	}
	if got := countIrrigationLogs(t, zone.ID); got != 1 {
		t.Errorf("灌溉记录应为 1 条，实际 %d", got)
	}
	// 预估 0.1 已按最小额度 10 预留并结算，已用为实际用水 0.1
	if usage := budgetUsage(t, budget.ID); usage.UsedAmount != 0.1 {
		t.Errorf("已用量应为 0.1，实际 %.2f", usage.UsedAmount)
	}
}

// 自动调度预估用水超上限时被拦截：不产生灌溉记录，超限告警只生成一次，
// 且不会被误判为灌溉执行失败。
func TestScheduledExceededBlockedWithoutNewLog(t *testing.T) {
	setup(t)
	zone := mustCreateZone(t, "调度超限拦截区")
	mustCreateBudget(t, zone.ID, 15, 80)
	// duration=200 → 预估用水 20，超过上限 15
	schedule := mustCreateSchedule(t, "长时灌溉", models.ScheduleTypeTimed, zone.ID, 200)

	s := scheduler.NewIrrigationScheduler()
	s.ExecuteIrrigation(schedule)

	if got := countIrrigationLogs(t, zone.ID); got != 0 {
		t.Errorf("超限被拦截时不应产生灌溉记录，实际 %d 条", got)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetExceeded); got != 1 {
		t.Errorf("超限告警应生成 1 条，实际 %d", got)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeIrrigationFailed); got != 0 {
		t.Errorf("预算拦截不应产生灌溉失败告警，实际 %d 条", got)
	}

	// 再次执行仍被拦截，超限告警本月不重复
	s.ExecuteIrrigation(schedule)
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetExceeded); got != 1 {
		t.Errorf("超限告警本月只应生成一次，实际 %d 条", got)
	}
	if got := countIrrigationLogs(t, zone.ID); got != 0 {
		t.Errorf("持续拦截不应产生灌溉记录，实际 %d 条", got)
	}
}

// ---------- 告警本月去重 ----------

// 用水量多次跨过告警阈值时，阈值告警本月只生成一次。
func TestThresholdAlertOnlyOncePerMonth(t *testing.T) {
	setup(t)
	irrigationSvc := services.NewIrrigationService()
	zone := mustCreateZone(t, "阈值告警区")
	mustCreateBudget(t, zone.ID, 100, 80)

	// 已用 50
	log1, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 50)
	if err != nil {
		t.Fatalf("首次触发失败: %v", err)
	}
	usage50 := 50.0
	if err := irrigationSvc.CompleteIrrigation(log1.ID, true, &usage50, nil); err != nil {
		t.Fatalf("完成灌溉失败: %v", err)
	}

	// 50+40=90，跨过 80% 阈值 → 生成 1 条阈值告警
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 40); err != nil {
		t.Fatalf("达到阈值不应拦截: %v", err)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetThreshold); got != 1 {
		t.Fatalf("达到阈值应生成 1 条告警，实际 %d", got)
	}

	// 再次触发（预估按最小额度 10 计，90+10=100 仍达阈值）→ 本月不重复告警
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 0.0001); err != nil {
		t.Fatalf("未超上限不应拦截: %v", err)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetThreshold); got != 1 {
		t.Errorf("阈值告警本月只应生成一次，实际 %d 条", got)
	}
}

// ---------- 停用 / 启用管控切换 ----------

// 预算停用后触发不再受管控（不检查、不预留），重新启用后恢复管控。
func TestBudgetDisableEnableControlSwitch(t *testing.T) {
	setup(t)
	irrigationSvc := services.NewIrrigationService()
	budgetSvc := services.NewBudgetService()
	zone := mustCreateZone(t, "管控切换区")
	budget := mustCreateBudget(t, zone.ID, 20, 80)

	// 启用中：预留 15
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 15); err != nil {
		t.Fatalf("首次触发失败: %v", err)
	}

	// 停用后：不再管控，触发成功且不新增预留
	if err := budgetSvc.DisableBudget(budget.ID); err != nil {
		t.Fatalf("停用预算失败: %v", err)
	}
	if got := budgetUsage(t, budget.ID).Status; got != models.BudgetStatusInactive {
		t.Errorf("停用后状态应为 inactive，实际 %s", got)
	}
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 15); err != nil {
		t.Errorf("预算停用后不应拦截: %v", err)
	}
	if usage := budgetUsage(t, budget.ID); usage.ReservedAmount != 15 {
		t.Errorf("停用期间不应新增预留，预留应为 15，实际 %.2f", usage.ReservedAmount)
	}

	// 重新启用：恢复管控，15+15=30 超上限 20 → 拦截
	if err := budgetSvc.EnableBudget(budget.ID); err != nil {
		t.Fatalf("启用预算失败: %v", err)
	}
	if got := budgetUsage(t, budget.ID).Status; got != models.BudgetStatusActive {
		t.Errorf("启用后状态应为 active，实际 %s", got)
	}
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 15); !errors.Is(err, services.ErrBudgetExceeded) {
		t.Errorf("重新启用后超限应拦截，实际 %v", err)
	}
}

// ---------- 并发创建预算 ----------

// 并发创建同一区域的预算：仅 1 个成功，其余稳定返回 ErrBudgetExists。
func TestConcurrentCreateBudgetOnlyOneSucceeds(t *testing.T) {
	setup(t)
	zone := mustCreateZone(t, "并发创建预算区")

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- services.NewBudgetService().CreateBudget(&models.WaterBudget{
				ZoneID:         zone.ID,
				MonthlyLimit:   100,
				AlertThreshold: 80,
			})
		}()
	}
	wg.Wait()
	close(results)

	var succeeded, conflicts, other int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, services.ErrBudgetExists):
			conflicts++
		default:
			other++
			t.Errorf("非预期错误（应稳定返回 ErrBudgetExists）: %v", err)
		}
	}
	if other > 0 {
		t.Fatalf("存在 %d 个非预期错误", other)
	}
	if succeeded != 1 || conflicts != workers-1 {
		t.Errorf("并发创建预算应仅 1 个成功：成功 %d，冲突 %d（期望 1/%d）", succeeded, conflicts, workers-1)
	}

	var count int64
	if err := database.DB.Model(&models.WaterBudget{}).
		Where("zone_id = ? AND status = ?", zone.ID, models.BudgetStatusActive).
		Count(&count).Error; err != nil {
		t.Fatalf("统计预算失败: %v", err)
	}
	if count != 1 {
		t.Errorf("区域内启用中的预算应只有 1 个，实际 %d", count)
	}
}

// ---------- 接口层：超限返回 409 Conflict ----------

// 通过 HTTP 接口走完整链路：创建区域与预算 → 手动灌溉占满预算 → 超限返回 409，
// 且不新增灌溉记录；未认证请求返回 401。
func TestAPIManualIrrigateConflictOnExceeded(t *testing.T) {
	router, token := setup(t)

	// 创建区域
	status, resp := doJSON(t, router, token, http.MethodPost, "/api/zones", map[string]interface{}{
		"name": "API回归区",
	})
	if status != http.StatusCreated {
		t.Fatalf("创建区域应返回 201，实际 %d: %s", status, resp.Message)
	}
	var zone models.IrrigationZone
	if err := json.Unmarshal(resp.Data, &zone); err != nil {
		t.Fatalf("解析区域响应失败: %v", err)
	}

	// 创建预算：上限 20
	status, resp = doJSON(t, router, token, http.MethodPost, "/api/budgets", map[string]interface{}{
		"zone_id":        zone.ID,
		"monthly_limit":  20,
		"alert_threshold": 80,
	})
	if status != http.StatusCreated {
		t.Fatalf("创建预算应返回 201，实际 %d: %s", status, resp.Message)
	}
	var budget models.WaterBudget
	if err := json.Unmarshal(resp.Data, &budget); err != nil {
		t.Fatalf("解析预算响应失败: %v", err)
	}

	// 两次手动灌溉（极小正数预估，各按最小额度 10 预留）→ 占满预算
	for i := 0; i < 2; i++ {
		status, resp = doJSON(t, router, token, http.MethodPost, "/api/irrigation/manual", map[string]interface{}{
			"zone_id":         zone.ID,
			"estimated_usage": 0.0001,
		})
		if status != http.StatusOK {
			t.Fatalf("第 %d 次手动灌溉应成功，实际状态 %d: %s", i+1, status, resp.Message)
		}
	}

	// 预算应被占满：预留 20，剩余 0
	status, resp = doJSON(t, router, token, http.MethodGet, fmt.Sprintf("/api/budgets/%d/usage", budget.ID), nil)
	if status != http.StatusOK {
		t.Fatalf("查询预算用量应返回 200，实际 %d", status)
	}
	var usage services.BudgetUsage
	if err := json.Unmarshal(resp.Data, &usage); err != nil {
		t.Fatalf("解析用量响应失败: %v", err)
	}
	if usage.ReservedAmount != 20 || usage.RemainingAmount != 0 {
		t.Errorf("预算应被占满（预留 20、剩余 0），实际 %+v", usage)
	}

	// 第三次触发超限 → 409 Conflict
	status, resp = doJSON(t, router, token, http.MethodPost, "/api/irrigation/manual", map[string]interface{}{
		"zone_id":         zone.ID,
		"estimated_usage": 0.0001,
	})
	if status != http.StatusConflict {
		t.Fatalf("超限应返回 409 Conflict，实际 %d: %s", status, resp.Message)
	}
	if resp.Code != http.StatusConflict {
		t.Errorf("响应体错误码应为 409，实际 %d", resp.Code)
	}

	// 被拦截时不应新增灌溉记录
	status, resp = doJSON(t, router, token, http.MethodGet,
		fmt.Sprintf("/api/irrigation/history?zone_id=%d", zone.ID), nil)
	if status != http.StatusOK {
		t.Fatalf("查询灌溉历史应返回 200，实际 %d", status)
	}
	var logs []models.IrrigationLog
	if err := json.Unmarshal(resp.Data, &logs); err != nil {
		t.Fatalf("解析灌溉历史失败: %v", err)
	}
	if len(logs) != 2 {
		t.Errorf("被拦截后灌溉记录应仍为 2 条，实际 %d 条", len(logs))
	}

	// 未认证请求 → 401
	status, _ = doJSON(t, router, "", http.MethodPost, "/api/irrigation/manual", map[string]interface{}{
		"zone_id": zone.ID,
	})
	if status != http.StatusUnauthorized {
		t.Errorf("未认证请求应返回 401，实际 %d", status)
	}
}
