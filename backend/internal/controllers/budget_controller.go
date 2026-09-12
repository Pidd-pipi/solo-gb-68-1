package controllers

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type BudgetController struct {
	budgetService *services.BudgetService
}

func NewBudgetController() *BudgetController {
	return &BudgetController{
		budgetService: services.NewBudgetService(),
	}
}

// ListBudgets godoc
// @Summary 获取用水预算列表
// @Description 获取所有区域的用水预算及本月实时用量（剩余量、用量占比）
// @Tags 用水预算
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param status query string false "预算状态 (active/inactive)"
// @Success 200 {array} services.BudgetUsage
// @Router /api/budgets [get]
func (c *BudgetController) List(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var status *string
	if statusStr := ctx.Query("status"); statusStr != "" {
		status = &statusStr
	}

	usages, err := c.budgetService.ListBudgetsWithUsage(zoneID, status)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, usages)
}

// GetBudget godoc
// @Summary 获取用水预算详情
// @Description 根据ID获取预算详情及本月实时用量
// @Tags 用水预算
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "预算ID"
// @Success 200 {object} services.BudgetUsage
// @Router /api/budgets/{id} [get]
func (c *BudgetController) Get(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	usage, err := c.budgetService.GetBudgetUsage(uint(id))
	if err != nil {
		if errors.Is(err, services.ErrBudgetNotFound) {
			response.NotFound(ctx, "Budget not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, usage)
}

// GetBudgetUsage godoc
// @Summary 获取预算实时用量
// @Description 获取预算本月实时用量：已用量、预留量、剩余量、用量占比
// @Tags 用水预算
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "预算ID"
// @Success 200 {object} services.BudgetUsage
// @Router /api/budgets/{id}/usage [get]
func (c *BudgetController) GetUsage(ctx *gin.Context) {
	c.Get(ctx)
}

type CreateBudgetRequest struct {
	ZoneID         uint    `json:"zone_id" binding:"required"`
	MonthlyLimit   float64 `json:"monthly_limit" binding:"required"`
	AlertThreshold float64 `json:"alert_threshold"`
}

type UpdateBudgetRequest struct {
	MonthlyLimit   *float64 `json:"monthly_limit"`
	AlertThreshold *float64 `json:"alert_threshold"`
}

// CreateBudget godoc
// @Summary 创建用水预算
// @Description 为区域创建月度用水预算（月度上限 + 告警阈值百分比），同一区域仅允许一个启用中的预算
// @Tags 用水预算
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param request body CreateBudgetRequest true "预算信息"
// @Success 201 {object} models.WaterBudget
// @Router /api/budgets [post]
func (c *BudgetController) Create(ctx *gin.Context) {
	var req CreateBudgetRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	threshold := req.AlertThreshold
	if threshold == 0 {
		threshold = 80
	}

	budget := &models.WaterBudget{
		ZoneID:         req.ZoneID,
		MonthlyLimit:   req.MonthlyLimit,
		AlertThreshold: threshold,
	}

	if err := c.budgetService.CreateBudget(budget); err != nil {
		if errors.Is(err, services.ErrBudgetExists) {
			response.Error(ctx, 409, err.Error())
			return
		}
		response.BadRequest(ctx, err.Error())
		return
	}

	response.Created(ctx, budget)
}

// UpdateBudget godoc
// @Summary 修改用水预算
// @Description 修改预算的月度上限和/或告警阈值
// @Tags 用水预算
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path int true "预算ID"
// @Param request body UpdateBudgetRequest true "更新信息"
// @Success 200 {object} response.Response
// @Router /api/budgets/{id} [put]
func (c *BudgetController) Update(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	var req UpdateBudgetRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.budgetService.UpdateBudget(uint(id), req.MonthlyLimit, req.AlertThreshold); err != nil {
		if errors.Is(err, services.ErrBudgetNotFound) {
			response.NotFound(ctx, "Budget not found")
			return
		}
		response.BadRequest(ctx, err.Error())
		return
	}
	response.Success(ctx, nil)
}

// DisableBudget godoc
// @Summary 停用用水预算
// @Description 停用后该区域触发灌溉不再受预算管控
// @Tags 用水预算
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "预算ID"
// @Success 200 {object} response.Response
// @Router /api/budgets/{id}/disable [post]
func (c *BudgetController) Disable(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	if err := c.budgetService.DisableBudget(uint(id)); err != nil {
		if errors.Is(err, services.ErrBudgetNotFound) {
			response.NotFound(ctx, "Budget not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, nil)
}

// EnableBudget godoc
// @Summary 启用用水预算
// @Description 重新启用已停用的预算
// @Tags 用水预算
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "预算ID"
// @Success 200 {object} response.Response
// @Router /api/budgets/{id}/enable [post]
func (c *BudgetController) Enable(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	if err := c.budgetService.EnableBudget(uint(id)); err != nil {
		if errors.Is(err, services.ErrBudgetNotFound) {
			response.NotFound(ctx, "Budget not found")
			return
		}
		if errors.Is(err, services.ErrBudgetExists) {
			response.Error(ctx, 409, err.Error())
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, nil)
}
