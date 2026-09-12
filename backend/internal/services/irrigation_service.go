package services

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

type IrrigationService struct{}

// DefaultEstimatedWaterUsage 触发灌溉未提供预估用水量时使用的默认预估值，
// 保证所有触发（包括未带预估用水的手动灌溉）都占用预算额度，
// 同一区域并发触发时预算不会被绕过。
const DefaultEstimatedWaterUsage = 10.0

func NewIrrigationService() *IrrigationService {
	return &IrrigationService{}
}

// StartIrrigation 触发灌溉。触发前在事务内检查区域用水预算：
//   - 超过月度上限：返回 ErrBudgetExceeded，事务回滚，不产生灌溉记录；
//   - 达到告警阈值：正常触发，同时创建告警提醒；
//   - 预算检查与预留在同一事务的预算行锁内完成，同一区域并发触发不会重复扣减预算。
//
// estimatedUsage 为本次灌溉预估用水（用于超限预判与并发预留）；
// 小于等于 0 时按 DefaultEstimatedWaterUsage 兜底，确保触发总是占用预算额度。
func (s *IrrigationService) StartIrrigation(scheduleID *uint, zoneID *uint, triggerType models.TriggerType, estimatedUsage float64) (*models.IrrigationLog, error) {
	if estimatedUsage <= 0 {
		estimatedUsage = DefaultEstimatedWaterUsage
	}

	log := &models.IrrigationLog{
		ScheduleID:  scheduleID,
		ZoneID:      zoneID,
		TriggerType: triggerType,
		StartTime:   time.Now(),
		Status:      models.ExecutionStatusInProgress,
	}

	budgetService := NewBudgetService()
	var check *BudgetCheckResult

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if zoneID != nil {
			result, err := budgetService.CheckBudget(tx, *zoneID, estimatedUsage)
			if err != nil {
				return err
			}
			check = result

			if err := tx.Create(log).Error; err != nil {
				return err
			}

			// 有启用预算时始终创建预留（预估量已兜底为正数），
			// 并发触发在预算行锁内串行扣减，不会绕过预算
			if result.Budget != nil {
				if err := budgetService.CreateReservation(tx, result.Budget.ID, log.ID, *zoneID, estimatedUsage); err != nil {
					return err
				}
			}
			return nil
		}
		return tx.Create(log).Error
	})
	if err != nil {
		if errors.Is(err, ErrBudgetExceeded) && zoneID != nil {
			budgetService.NotifyExceeded(*zoneID, err.Error())
		}
		return nil, err
	}

	if check != nil && check.ThresholdReached && check.Budget != nil && zoneID != nil {
		budgetService.NotifyThresholdReached(*zoneID, check)
	}

	return log, nil
}

func (s *IrrigationService) CompleteIrrigation(logID uint, success bool, waterUsage *float64, errorMsg *string) error {
	now := time.Now()
	updates := map[string]interface{}{
		"end_time": now,
	}

	if success {
		updates["status"] = models.ExecutionStatusSuccess
	} else {
		updates["status"] = models.ExecutionStatusFailed
		if errorMsg != nil {
			updates["error_message"] = *errorMsg
		}
	}

	if waterUsage != nil {
		updates["water_usage"] = *waterUsage
	}

	budgetService := NewBudgetService()

	return database.DB.Transaction(func(tx *gorm.DB) error {
		// 锁定区域预算行，与 StartIrrigation 的预算检查串行化，保证用量统计口径一致
		var log models.IrrigationLog
		if err := tx.Select("id", "zone_id").First(&log, logID).Error; err != nil {
			return err
		}
		if log.ZoneID != nil {
			if _, err := budgetService.lockActiveBudget(tx, *log.ZoneID); err != nil {
				return err
			}
		}

		if err := tx.Model(&models.IrrigationLog{}).
			Where("id = ?", logID).
			Updates(updates).Error; err != nil {
			return err
		}

		// 结算本次灌溉的预算预留（幂等）
		return budgetService.SettleReservation(tx, logID, success)
	})
}

func (s *IrrigationService) GetIrrigationHistory(zoneID *uint, startTime, endTime time.Time, limit int) ([]models.IrrigationLog, error) {
	var logs []models.IrrigationLog
	query := database.DB

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if !startTime.IsZero() {
		query = query.Where("start_time >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("start_time <= ?", endTime)
	}

	if limit > 0 {
		query = query.Limit(limit)
	}

	if err := query.Order("start_time DESC").Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

type WaterUsageStats struct {
	TotalUsage      float64 `json:"total_usage"`
	Duration        int64   `json:"duration"`
	IrrigationCount int64   `json:"irrigation_count"`
}

func (s *IrrigationService) GetWaterUsageStats(zoneID *uint, startTime, endTime time.Time) (*WaterUsageStats, error) {
	var stats WaterUsageStats
	query := database.DB.Model(&models.IrrigationLog{}).
		Select("COALESCE(SUM(water_usage), 0) as total_usage, COALESCE(COUNT(*), 0) as irrigation_count").
		Where("status = ?", models.ExecutionStatusSuccess)

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if !startTime.IsZero() {
		query = query.Where("start_time >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("start_time <= ?", endTime)
	}

	err := query.Scan(&stats).Error
	return &stats, err
}

type ZoneWaterUsage struct {
	ZoneID     uint    `json:"zone_id"`
	ZoneName   string  `json:"zone_name"`
	WaterUsage float64 `json:"water_usage"`
	Percentage float64 `json:"percentage"`
}

func (s *IrrigationService) GetZoneWaterUsage(startTime, endTime time.Time) ([]ZoneWaterUsage, error) {
	var zoneUsages []ZoneWaterUsage

	query := `
		SELECT
			z.id as zone_id,
			z.name as zone_name,
			COALESCE(SUM(il.water_usage), 0) as water_usage
		FROM irrigation_zones z
		LEFT JOIN irrigation_logs il ON z.id = il.zone_id
			AND il.status = 'success'
			AND il.start_time >= ?
			AND il.start_time <= ?
		GROUP BY z.id, z.name
		ORDER BY water_usage DESC
	`

	err := database.DB.Raw(query, startTime, endTime).Scan(&zoneUsages).Error
	if err != nil {
		return nil, err
	}

	var total float64
	for _, zu := range zoneUsages {
		total += zu.WaterUsage
	}

	if total > 0 {
		for i := range zoneUsages {
			zoneUsages[i].Percentage = (zoneUsages[i].WaterUsage / total) * 100
		}
	}

	return zoneUsages, nil
}
