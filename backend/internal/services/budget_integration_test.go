package services

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 集成测试：需要可用的 PostgreSQL（默认 127.0.0.1:55432，可用 TEST_PG_DSN 覆盖）。
// 未设置 TEST_PG_DSN 且默认地址不可达时自动跳过，不影响普通 go test。
func setupIntegrationDB(t *testing.T) {
	t.Helper()

	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		dsn = "host=127.0.0.1 port=55432 user=postgres dbname=irrigation sslmode=disable"
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Skipf("无法连接测试数据库，跳过集成测试: %v", err)
	}

	// 重置 schema 并加载 init.sql
	if err := db.Exec("DROP SCHEMA public CASCADE").Error; err != nil {
		t.Fatalf("重置 schema 失败: %v", err)
	}
	if err := db.Exec("CREATE SCHEMA public").Error; err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}

	sqlBytes, err := os.ReadFile("../../../database/init.sql")
	if err != nil {
		t.Fatalf("读取 init.sql 失败: %v", err)
	}
	for _, stmt := range strings.Split(string(sqlBytes), ";\n") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("执行 init.sql 语句失败: %v\n语句: %s", err, stmt)
		}
	}

	database.DB = db
	t.Cleanup(func() { database.DB = nil })
}

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
	if err := NewBudgetService().CreateBudget(budget); err != nil {
		t.Fatalf("创建预算失败: %v", err)
	}
	return *budget
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

func TestBudgetCRUD(t *testing.T) {
	setupIntegrationDB(t)
	svc := NewBudgetService()
	zone := mustCreateZone(t, "玫瑰园")

	// 参数校验
	if err := svc.CreateBudget(&models.WaterBudget{ZoneID: zone.ID, MonthlyLimit: 0, AlertThreshold: 80}); err == nil {
		t.Error("monthly_limit 为 0 应报错")
	}
	if err := svc.CreateBudget(&models.WaterBudget{ZoneID: zone.ID, MonthlyLimit: 100, AlertThreshold: 120}); err == nil {
		t.Error("alert_threshold 超过 100 应报错")
	}
	if err := svc.CreateBudget(&models.WaterBudget{ZoneID: 99999, MonthlyLimit: 100, AlertThreshold: 80}); err == nil {
		t.Error("区域不存在应报错")
	}

	// 创建
	budget := mustCreateBudget(t, zone.ID, 100, 80)
	if budget.Status != models.BudgetStatusActive {
		t.Errorf("新预算状态应为 active，实际 %s", budget.Status)
	}

	// 同一区域重复创建启用预算应失败
	if err := svc.CreateBudget(&models.WaterBudget{ZoneID: zone.ID, MonthlyLimit: 200, AlertThreshold: 80}); !errors.Is(err, ErrBudgetExists) {
		t.Errorf("重复创建应返回 ErrBudgetExists，实际 %v", err)
	}

	// 查询
	usage, err := svc.GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询预算用量失败: %v", err)
	}
	if usage.MonthlyLimit != 100 || usage.RemainingAmount != 100 || usage.UsagePercent != 0 {
		t.Errorf("初始用量不正确: %+v", usage)
	}

	// 修改
	newLimit, newThreshold := 200.0, 90.0
	if err := svc.UpdateBudget(budget.ID, &newLimit, &newThreshold); err != nil {
		t.Fatalf("修改预算失败: %v", err)
	}
	updated, _ := svc.GetBudgetByID(budget.ID)
	if updated.MonthlyLimit != 200 || updated.AlertThreshold != 90 {
		t.Errorf("修改未生效: %+v", updated)
	}
	badLimit := -1.0
	if err := svc.UpdateBudget(budget.ID, &badLimit, nil); err == nil {
		t.Error("monthly_limit 为负应报错")
	}

	// 停用 / 启用
	if err := svc.DisableBudget(budget.ID); err != nil {
		t.Fatalf("停用预算失败: %v", err)
	}
	disabled, _ := svc.GetBudgetByID(budget.ID)
	if disabled.Status != models.BudgetStatusInactive {
		t.Errorf("停用后状态应为 inactive，实际 %s", disabled.Status)
	}
	if err := svc.EnableBudget(budget.ID); err != nil {
		t.Fatalf("启用预算失败: %v", err)
	}
	enabled, _ := svc.GetBudgetByID(budget.ID)
	if enabled.Status != models.BudgetStatusActive {
		t.Errorf("启用后状态应为 active，实际 %s", enabled.Status)
	}
}

func TestBudgetCheckAndAlerts(t *testing.T) {
	setupIntegrationDB(t)
	irrigationSvc := NewIrrigationService()
	budgetSvc := NewBudgetService()
	zone := mustCreateZone(t, "草坪区")
	budget := mustCreateBudget(t, zone.ID, 100, 80)

	// 触发并完成一次灌溉：用水 50
	log1, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 0)
	if err != nil {
		t.Fatalf("首次触发灌溉失败: %v", err)
	}
	usage50 := 50.0
	if err := irrigationSvc.CompleteIrrigation(log1.ID, true, &usage50, nil); err != nil {
		t.Fatalf("完成灌溉失败: %v", err)
	}

	usage, err := budgetSvc.GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if usage.UsedAmount != 50 || usage.RemainingAmount != 50 || usage.UsagePercent != 50 {
		t.Errorf("用量统计不正确: %+v", usage)
	}

	// 触发第二次：预估 40，累计将达 90，超过 80% 阈值 → 允许但产生告警
	log2, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 40)
	if err != nil {
		t.Fatalf("达到阈值时不应拦截: %v", err)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetThreshold); got != 1 {
		t.Errorf("达到阈值应产生 1 条告警，实际 %d", got)
	}
	usage40 := 40.0
	if err := irrigationSvc.CompleteIrrigation(log2.ID, true, &usage40, nil); err != nil {
		t.Fatalf("完成灌溉失败: %v", err)
	}

	// 重复触发不重复产生阈值告警
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 0); err != nil {
		t.Fatalf("未超上限不应拦截: %v", err)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetThreshold); got != 1 {
		t.Errorf("阈值告警本月不应重复创建，实际 %d 条", got)
	}

	// 已用 90，预估 20 将超上限 → 拦截且不产生新灌溉记录
	before := countIrrigationLogs(t, zone.ID)
	_, err = irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 20)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("超上限应返回 ErrBudgetExceeded，实际 %v", err)
	}
	if after := countIrrigationLogs(t, zone.ID); after != before {
		t.Errorf("被拦截时不应产生新灌溉记录，拦截前 %d 条，拦截后 %d 条", before, after)
	}
	if got := countAlerts(t, zone.ID, models.AlertTypeBudgetExceeded); got != 1 {
		t.Errorf("超限拦截应产生 1 条告警，实际 %d", got)
	}

	// 停用预算后不再管控
	if err := budgetSvc.DisableBudget(budget.ID); err != nil {
		t.Fatalf("停用预算失败: %v", err)
	}
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 0); err != nil {
		t.Errorf("预算停用后不应拦截: %v", err)
	}
}

func TestBudgetConcurrentTriggers(t *testing.T) {
	setupIntegrationDB(t)
	irrigationSvc := NewIrrigationService()
	zone := mustCreateZone(t, "灌木区")
	budget := mustCreateBudget(t, zone.ID, 100, 80)

	// 10 个并发触发，每个预估用水 30：预算仅允许 3 个（3*30=90 <= 100），其余必须被拦截
	const workers = 10
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeTimed, 30)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var succeeded, blocked, other int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrBudgetExceeded):
			blocked++
		default:
			other++
			t.Errorf("非预期错误: %v", err)
		}
	}
	if other > 0 {
		t.Fatalf("存在 %d 个非预期错误", other)
	}
	if succeeded != 3 || blocked != 7 {
		t.Errorf("并发扣减不正确：成功 %d（期望 3），拦截 %d（期望 7）", succeeded, blocked)
	}

	// 预留总额应等于 3 * 30 = 90，即预算未被重复扣减
	usage, err := NewBudgetService().GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if usage.ReservedAmount != 90 {
		t.Errorf("预留总额应为 90，实际 %.2f", usage.ReservedAmount)
	}
	if usage.RemainingAmount != 10 {
		t.Errorf("剩余量应为 10，实际 %.2f", usage.RemainingAmount)
	}

	// 完成全部进行中的灌溉（每次实际用水 30），结算后已用 90、预留归零
	var logs []models.IrrigationLog
	if err := database.DB.Where("zone_id = ? AND status = ?", zone.ID, models.ExecutionStatusInProgress).Find(&logs).Error; err != nil {
		t.Fatalf("查询灌溉记录失败: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("进行中的灌溉应为 3 条，实际 %d", len(logs))
	}
	for _, l := range logs {
		usage30 := 30.0
		if err := irrigationSvc.CompleteIrrigation(l.ID, true, &usage30, nil); err != nil {
			t.Fatalf("完成灌溉失败: %v", err)
		}
	}

	usage, err = NewBudgetService().GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if usage.UsedAmount != 90 || usage.ReservedAmount != 0 || usage.RemainingAmount != 10 {
		t.Errorf("结算后用量不正确: %+v", usage)
	}

	// 重复结算同一记录不应重复扣减
	for _, l := range logs {
		usage30 := 30.0
		if err := irrigationSvc.CompleteIrrigation(l.ID, true, &usage30, nil); err != nil {
			t.Fatalf("重复完成灌溉失败: %v", err)
		}
	}
	usage, err = NewBudgetService().GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if usage.UsedAmount != 90 {
		t.Errorf("重复结算后已用量应为 90，实际 %.2f", usage.UsedAmount)
	}

	// 已用 90，再触发预估 30 必被拦截
	if _, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeTimed, 30); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("已用 90 时预估 30 应被拦截，实际 %v", err)
	}
}

func TestBudgetConcurrentTriggersWithoutEstimate(t *testing.T) {
	setupIntegrationDB(t)
	irrigationSvc := NewIrrigationService()
	zone := mustCreateZone(t, "无预估用水区")
	budget := mustCreateBudget(t, zone.ID, 50, 80)

	// 10 个并发触发均不带预估用水：按默认预估值 10 扣减，
	// 上限 50 仅允许 5 个通过，其余必须被拦截
	const workers = 10
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var succeeded, blocked, other int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrBudgetExceeded):
			blocked++
		default:
			other++
			t.Errorf("非预期错误: %v", err)
		}
	}
	if other > 0 {
		t.Fatalf("存在 %d 个非预期错误", other)
	}
	if succeeded != 5 || blocked != 5 {
		t.Errorf("无预估用水时并发扣减不正确：成功 %d（期望 5），拦截 %d（期望 5）", succeeded, blocked)
	}

	// 预算应被占满：预留 5*10=50，剩余 0
	usage, err := NewBudgetService().GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if usage.ReservedAmount != 50 || usage.RemainingAmount != 0 {
		t.Errorf("无预估用水时预算未被占用: %+v", usage)
	}
}

func TestBudgetConcurrentTriggersWithTinyEstimate(t *testing.T) {
	setupIntegrationDB(t)
	irrigationSvc := NewIrrigationService()
	zone := mustCreateZone(t, "极小预估用水区")
	budget := mustCreateBudget(t, zone.ID, 50, 80)

	// 10 个并发触发均传入极小正数预估用水：应按最小预留额度 10 扣减，
	// 上限 50 仅允许 5 个通过，其余必须被拦截，预算保护不被绕过
	const workers = 10
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeManual, 0.0001)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var succeeded, blocked, other int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrBudgetExceeded):
			blocked++
		default:
			other++
			t.Errorf("非预期错误: %v", err)
		}
	}
	if other > 0 {
		t.Fatalf("存在 %d 个非预期错误", other)
	}
	if succeeded != 5 || blocked != 5 {
		t.Errorf("极小预估用水时并发扣减不正确：成功 %d（期望 5），拦截 %d（期望 5）", succeeded, blocked)
	}

	// 预算应被占满：预留 5*10=50，剩余 0
	usage, err := NewBudgetService().GetBudgetUsage(budget.ID)
	if err != nil {
		t.Fatalf("查询用量失败: %v", err)
	}
	if usage.ReservedAmount != 50 || usage.RemainingAmount != 0 {
		t.Errorf("极小预估用水时预算未被足额占用: %+v", usage)
	}
}

func TestBudgetConcurrentCreate(t *testing.T) {
	setupIntegrationDB(t)
	svc := NewBudgetService()
	zone := mustCreateZone(t, "并发预算区")

	// 并发创建同一区域的首条启用预算：仅 1 个成功，其余稳定返回 ErrBudgetExists（HTTP 409）
	const workers = 5
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- svc.CreateBudget(&models.WaterBudget{
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
		case errors.Is(err, ErrBudgetExists):
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
		t.Errorf("并发创建预算不正确：成功 %d（期望 1），冲突 %d（期望 %d）", succeeded, conflicts, workers-1)
	}
}

func TestReservationSettleAndRelease(t *testing.T) {
	setupIntegrationDB(t)
	irrigationSvc := NewIrrigationService()
	budgetSvc := NewBudgetService()
	zone := mustCreateZone(t, "花坛")
	budget := mustCreateBudget(t, zone.ID, 100, 80)

	// 灌溉失败：预留应被释放，不计入用量
	log1, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeTimed, 60)
	if err != nil {
		t.Fatalf("触发灌溉失败: %v", err)
	}
	usage, _ := budgetSvc.GetBudgetUsage(budget.ID)
	if usage.ReservedAmount != 60 {
		t.Errorf("触发后预留应为 60，实际 %.2f", usage.ReservedAmount)
	}
	if err := irrigationSvc.CompleteIrrigation(log1.ID, false, nil, nil); err != nil {
		t.Fatalf("完成灌溉失败: %v", err)
	}
	usage, _ = budgetSvc.GetBudgetUsage(budget.ID)
	if usage.ReservedAmount != 0 || usage.UsedAmount != 0 {
		t.Errorf("失败后预留应释放且用量为 0: %+v", usage)
	}

	// 过期预留由定时清理释放
	log2, err := irrigationSvc.StartIrrigation(nil, &zone.ID, models.TriggerTypeTimed, 60)
	if err != nil {
		t.Fatalf("触发灌溉失败: %v", err)
	}
	if _, err := budgetSvc.ReleaseStaleReservations(time.Hour); err != nil {
		t.Fatalf("清理过期预留失败: %v", err)
	}
	usage, _ = budgetSvc.GetBudgetUsage(budget.ID)
	if usage.ReservedAmount != 60 {
		t.Errorf("未过期的预留不应被清理，实际 %.2f", usage.ReservedAmount)
	}
	// 将预留时间拨到 3 小时前再清理
	if err := database.DB.Model(&models.WaterBudgetReservation{}).
		Where("log_id = ?", log2.ID).
		Update("created_at", time.Now().Add(-3*time.Hour)).Error; err != nil {
		t.Fatalf("修改预留时间失败: %v", err)
	}
	released, err := budgetSvc.ReleaseStaleReservations(2 * time.Hour)
	if err != nil || released != 1 {
		t.Fatalf("应释放 1 条过期预留，released=%d, err=%v", released, err)
	}
	usage, _ = budgetSvc.GetBudgetUsage(budget.ID)
	if usage.ReservedAmount != 0 {
		t.Errorf("过期预留应被释放，实际预留 %.2f", usage.ReservedAmount)
	}
}
