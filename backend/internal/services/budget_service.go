package services

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

var (
	// ErrBudgetExceeded 触发灌溉时本月用水已达到/将超过月度上限
	ErrBudgetExceeded = errors.New("water budget exceeded")
	// ErrBudgetNotFound 预算不存在
	ErrBudgetNotFound = errors.New("budget not found")
	// ErrBudgetExists 同一区域已存在启用中的预算
	ErrBudgetExists = errors.New("an active budget already exists for this zone")
)

type BudgetService struct{}

func NewBudgetService() *BudgetService {
	return &BudgetService{}
}

// isUniqueViolation 判断是否为 PostgreSQL 唯一约束冲突（SQLSTATE 23505）。
// 并发创建/启用同一区域的预算时，未通过"已存在"预检的请求会撞上
// idx_water_budgets_zone_active 部分唯一索引，需稳定映射为 ErrBudgetExists。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------- 纯函数：预算判定与用量计算（便于单元测试） ----------

// evaluateBudget 判定是否允许触发灌溉。
// used: 本月已完成灌溉累计用水；reserved: 进行中灌溉的预留用水；
// estimate: 本次灌溉预估用水（未知为 0）；limit: 月度上限；thresholdPct: 告警阈值百分比。
// 返回：是否允许、是否达到告警阈值、拦截原因。
func evaluateBudget(used, reserved, estimate, limit, thresholdPct float64) (allowed bool, thresholdReached bool, reason string) {
	effective := used + reserved
	if effective >= limit {
		return false, true, fmt.Sprintf("本月累计用水 %.2f 已达到月度上限 %.2f", effective, limit)
	}
	if estimate > 0 && effective+estimate > limit {
		return false, true, fmt.Sprintf("本次灌溉预计用水 %.2f，叠加本月已用及预留 %.2f 后将超过月度上限 %.2f", estimate, effective, limit)
	}
	projected := effective + estimate
	thresholdReached = limit > 0 && projected*100 >= thresholdPct*limit
	return true, thresholdReached, ""
}

// computeUsage 计算剩余量与用量占比（占比含已用与预留，与剩余量口径一致）。
func computeUsage(limit, used, reserved float64) (remaining float64, percent float64) {
	remaining = limit - used - reserved
	if remaining < 0 {
		remaining = 0
	}
	if limit > 0 {
		percent = (used + reserved) / limit * 100
	}
	return remaining, percent
}

func currentPeriod(t time.Time) string {
	return t.Format("2006-01")
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

// ---------- 预算 CRUD ----------

func (s *BudgetService) CreateBudget(budget *models.WaterBudget) error {
	if budget.MonthlyLimit <= 0 {
		return errors.New("monthly_limit 必须大于 0")
	}
	if budget.AlertThreshold <= 0 || budget.AlertThreshold > 100 {
		return errors.New("alert_threshold 必须在 (0, 100] 之间")
	}

	var zone models.IrrigationZone
	if err := database.DB.First(&zone, budget.ZoneID).Error; err != nil {
		return errors.New("zone not found")
	}

	var count int64
	if err := database.DB.Model(&models.WaterBudget{}).
		Where("zone_id = ? AND status = ?", budget.ZoneID, models.BudgetStatusActive).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrBudgetExists
	}

	budget.Status = models.BudgetStatusActive
	if err := database.DB.Create(budget).Error; err != nil {
		// 并发创建同一区域的首条启用预算时，预检与插入之间存在竞争窗口，
		// 撞上唯一索引的请求统一返回 ErrBudgetExists（HTTP 409）
		if isUniqueViolation(err) {
			return ErrBudgetExists
		}
		return err
	}
	return nil
}

func (s *BudgetService) GetBudgetByID(id uint) (*models.WaterBudget, error) {
	var budget models.WaterBudget
	if err := database.DB.Preload("Zone").First(&budget, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrBudgetNotFound
		}
		return nil, err
	}
	return &budget, nil
}

func (s *BudgetService) ListBudgets(zoneID *uint, status *string) ([]models.WaterBudget, error) {
	var budgets []models.WaterBudget
	query := database.DB.Preload("Zone")

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if status != nil {
		query = query.Where("status = ?", *status)
	}

	if err := query.Order("id ASC").Find(&budgets).Error; err != nil {
		return nil, err
	}
	return budgets, nil
}

func (s *BudgetService) UpdateBudget(id uint, monthlyLimit *float64, alertThreshold *float64) error {
	updates := map[string]interface{}{}

	if monthlyLimit != nil {
		if *monthlyLimit <= 0 {
			return errors.New("monthly_limit 必须大于 0")
		}
		updates["monthly_limit"] = *monthlyLimit
	}
	if alertThreshold != nil {
		if *alertThreshold <= 0 || *alertThreshold > 100 {
			return errors.New("alert_threshold 必须在 (0, 100] 之间")
		}
		updates["alert_threshold"] = *alertThreshold
	}
	if len(updates) == 0 {
		return errors.New("没有需要更新的字段")
	}

	result := database.DB.Model(&models.WaterBudget{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrBudgetNotFound
	}
	return nil
}

func (s *BudgetService) DisableBudget(id uint) error {
	result := database.DB.Model(&models.WaterBudget{}).
		Where("id = ?", id).
		Update("status", models.BudgetStatusInactive)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrBudgetNotFound
	}
	return nil
}

func (s *BudgetService) EnableBudget(id uint) error {
	budget, err := s.GetBudgetByID(id)
	if err != nil {
		return err
	}

	var count int64
	if err := database.DB.Model(&models.WaterBudget{}).
		Where("zone_id = ? AND status = ? AND id <> ?", budget.ZoneID, models.BudgetStatusActive, id).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrBudgetExists
	}

	// 并发启用同一区域的多个预算时同样依赖唯一索引兜底
	if err := database.DB.Model(&models.WaterBudget{}).
		Where("id = ?", id).
		Update("status", models.BudgetStatusActive).Error; err != nil {
		if isUniqueViolation(err) {
			return ErrBudgetExists
		}
		return err
	}
	return nil
}

// ---------- 实时用量 ----------

// BudgetUsage 区域预算的实时用量视图
type BudgetUsage struct {
	BudgetID         uint                `json:"budget_id"`
	ZoneID           uint                `json:"zone_id"`
	ZoneName         string              `json:"zone_name,omitempty"`
	Period           string              `json:"period"` // 统计月份，格式 2006-01
	Status           models.BudgetStatus `json:"status"`
	MonthlyLimit     float64             `json:"monthly_limit"`
	AlertThreshold   float64             `json:"alert_threshold"`
	UsedAmount       float64             `json:"used_amount"`      // 本月已完成灌溉累计用水
	ReservedAmount   float64             `json:"reserved_amount"`  // 进行中灌溉的预留用水
	RemainingAmount  float64             `json:"remaining_amount"` // 剩余可用量
	UsagePercent     float64             `json:"usage_percent"`    // 用量占比(%)，含预留
	ThresholdReached bool                `json:"threshold_reached"`
	Exceeded         bool                `json:"exceeded"`
}

func (s *BudgetService) buildUsage(budget *models.WaterBudget) *BudgetUsage {
	now := time.Now()
	used, reserved, err := s.getMonthUsage(database.DB, budget.ZoneID, currentPeriod(now), monthStart(now))
	if err != nil {
		used, reserved = 0, 0
	}

	remaining, percent := computeUsage(budget.MonthlyLimit, used, reserved)
	usage := &BudgetUsage{
		BudgetID:        budget.ID,
		ZoneID:          budget.ZoneID,
		Period:          currentPeriod(now),
		Status:          budget.Status,
		MonthlyLimit:    budget.MonthlyLimit,
		AlertThreshold:  budget.AlertThreshold,
		UsedAmount:      used,
		ReservedAmount:  reserved,
		RemainingAmount: remaining,
		UsagePercent:    percent,
		Exceeded:        used+reserved >= budget.MonthlyLimit,
	}
	if budget.Zone != nil {
		usage.ZoneName = budget.Zone.Name
	}
	_, usage.ThresholdReached, _ = evaluateBudget(used, reserved, 0, budget.MonthlyLimit, budget.AlertThreshold)
	return usage
}

// GetBudgetUsage 查询单个预算的实时用量（剩余量、用量占比）
func (s *BudgetService) GetBudgetUsage(id uint) (*BudgetUsage, error) {
	budget, err := s.GetBudgetByID(id)
	if err != nil {
		return nil, err
	}
	return s.buildUsage(budget), nil
}

// ListBudgetsWithUsage 列出预算并附带实时用量
func (s *BudgetService) ListBudgetsWithUsage(zoneID *uint, status *string) ([]BudgetUsage, error) {
	budgets, err := s.ListBudgets(zoneID, status)
	if err != nil {
		return nil, err
	}

	usages := make([]BudgetUsage, 0, len(budgets))
	for i := range budgets {
		usages = append(usages, *s.buildUsage(&budgets[i]))
	}
	return usages, nil
}

// ---------- 触发前预算检查与并发控制 ----------

// BudgetCheckResult 预算检查结果
type BudgetCheckResult struct {
	Budget           *models.WaterBudget // 无启用预算时为 nil（不管控）
	UsedAmount       float64
	ReservedAmount   float64
	EstimatedUsage   float64
	ThresholdReached bool
}

// lockActiveBudget 在事务内以行锁（FOR UPDATE）锁定区域当前启用的预算，
// 使同一区域的并发灌溉触发串行化，防止预算被重复扣减。无启用预算时返回 nil。
func (s *BudgetService) lockActiveBudget(tx *gorm.DB, zoneID uint) (*models.WaterBudget, error) {
	var budget models.WaterBudget
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("zone_id = ? AND status = ?", zoneID, models.BudgetStatusActive).
		First(&budget).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &budget, nil
}

// getMonthUsage 统计本月已结算用水（已完成灌溉）与预留用水（进行中灌溉）。
// 只统计对应灌溉记录仍处于进行中的预留，避免已结束灌溉的预留被重复计算。
func (s *BudgetService) getMonthUsage(tx *gorm.DB, zoneID uint, period string, since time.Time) (used float64, reserved float64, err error) {
	err = tx.Model(&models.IrrigationLog{}).
		Where("zone_id = ? AND status = ? AND start_time >= ?", zoneID, models.ExecutionStatusSuccess, since).
		Select("COALESCE(SUM(water_usage), 0)").
		Scan(&used).Error
	if err != nil {
		return 0, 0, err
	}

	err = tx.Model(&models.WaterBudgetReservation{}).
		Joins("JOIN irrigation_logs ON irrigation_logs.id = water_budget_reservations.log_id").
		Where("water_budget_reservations.zone_id = ? AND water_budget_reservations.period = ? AND water_budget_reservations.status = ? AND irrigation_logs.status = ?",
			zoneID, period, models.ReservationStatusReserved, models.ExecutionStatusInProgress).
		Select("COALESCE(SUM(water_budget_reservations.amount), 0)").
		Scan(&reserved).Error
	if err != nil {
		return 0, 0, err
	}
	return used, reserved, nil
}

// CheckBudget 在事务内检查区域预算。预算超限时返回 ErrBudgetExceeded（调用方回滚，不产生灌溉记录）。
func (s *BudgetService) CheckBudget(tx *gorm.DB, zoneID uint, estimatedUsage float64) (*BudgetCheckResult, error) {
	now := time.Now()

	budget, err := s.lockActiveBudget(tx, zoneID)
	if err != nil {
		return nil, err
	}
	if budget == nil {
		return &BudgetCheckResult{}, nil
	}

	used, reserved, err := s.getMonthUsage(tx, zoneID, currentPeriod(now), monthStart(now))
	if err != nil {
		return nil, err
	}

	allowed, thresholdReached, reason := evaluateBudget(used, reserved, estimatedUsage, budget.MonthlyLimit, budget.AlertThreshold)
	if !allowed {
		return nil, fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
	}

	return &BudgetCheckResult{
		Budget:           budget,
		UsedAmount:       used,
		ReservedAmount:   reserved,
		EstimatedUsage:   estimatedUsage,
		ThresholdReached: thresholdReached,
	}, nil
}

// CreateReservation 在事务内为灌溉记录创建用水预留（须在 CheckBudget 之后、同一事务中调用）。
func (s *BudgetService) CreateReservation(tx *gorm.DB, budgetID uint, logID uint, zoneID uint, amount float64) error {
	reservation := &models.WaterBudgetReservation{
		BudgetID: budgetID,
		LogID:    logID,
		ZoneID:   zoneID,
		Amount:   amount,
		Period:   currentPeriod(time.Now()),
		Status:   models.ReservationStatusReserved,
	}
	return tx.Create(reservation).Error
}

// SettleReservation 灌溉结束时结算预留：成功转为 settled，失败释放为 released。
// 只影响仍处于 reserved 状态的记录，重复调用幂等，不会重复扣减。
func (s *BudgetService) SettleReservation(tx *gorm.DB, logID uint, success bool) error {
	status := models.ReservationStatusSettled
	if !success {
		status = models.ReservationStatusReleased
	}
	return tx.Model(&models.WaterBudgetReservation{}).
		Where("log_id = ? AND status = ?", logID, models.ReservationStatusReserved).
		Updates(map[string]interface{}{
			"status":     status,
			"updated_at": time.Now(),
		}).Error
}

// ReleaseStaleReservations 释放长时间未结算的预留（如灌溉记录异常中断），避免额度泄漏。
func (s *BudgetService) ReleaseStaleReservations(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result := database.DB.Model(&models.WaterBudgetReservation{}).
		Where("status = ? AND created_at < ?", models.ReservationStatusReserved, cutoff).
		Updates(map[string]interface{}{
			"status":     models.ReservationStatusReleased,
			"updated_at": time.Now(),
		})
	return result.RowsAffected, result.Error
}

// ---------- 预算告警 ----------

// NotifyThresholdReached 用量达到告警阈值时创建告警（本月内同区域未处理告警不重复创建）。
func (s *BudgetService) NotifyThresholdReached(zoneID uint, check *BudgetCheckResult) {
	if check == nil || check.Budget == nil {
		return
	}
	percent := 0.0
	if check.Budget.MonthlyLimit > 0 {
		percent = (check.UsedAmount + check.ReservedAmount + check.EstimatedUsage) / check.Budget.MonthlyLimit * 100
	}
	zoneName := s.zoneName(zoneID)
	_ = NewAlertService().CreateBudgetThresholdAlert(zoneID, zoneName, percent, check.Budget.AlertThreshold)
}

// NotifyExceeded 预算超限被拦截时创建告警。
func (s *BudgetService) NotifyExceeded(zoneID uint, reason string) {
	zoneName := s.zoneName(zoneID)
	_ = NewAlertService().CreateBudgetExceededAlert(zoneID, zoneName, reason)
}

func (s *BudgetService) zoneName(zoneID uint) string {
	var zone models.IrrigationZone
	if err := database.DB.First(&zone, zoneID).Error; err != nil {
		return fmt.Sprintf("#%d", zoneID)
	}
	return zone.Name
}
