package eebus

import (
	ucapi "github.com/enbility/eebus-go/usecases/api"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/julienar/eebus-charged/internal/config"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// iec61851Controller handles charging via IEC 61851 using OPEV (obligation limits)
type iec61851Controller struct {
	chargingCfg    *config.ChargingConfig
	opEV           ucapi.CemOPEVInterface
	limitsProvider LimitsProvider
	logger         *zap.Logger
}

func newIec61851Controller(
	chargingCfg *config.ChargingConfig,
	opEV ucapi.CemOPEVInterface,
	limitsProvider LimitsProvider,
	logger *zap.Logger,
) *iec61851Controller {
	return &iec61851Controller{
		chargingCfg:    chargingCfg,
		opEV:           opEV,
		limitsProvider: limitsProvider,
		logger:         logger.With(zap.String("controller", "iec61851")),
	}
}

func (c *iec61851Controller) Name() string {
	return "iec61851"
}

func (c *iec61851Controller) GetCapabilities(evEntity spineapi.EntityRemoteInterface) ControllerCapabilities {
	opevAvailable := false
	if evEntity != nil {
		opevAvailable = c.opEV.IsScenarioAvailableAtEntity(evEntity, 1)
	}

	return ControllerCapabilities{
		CommunicationStandard: "iec61851",
		ControllerType:        "iec61851",
		OPEVAvailable:         &opevAvailable,
		// VAS and OSCEV not applicable for IEC 61851
	}
}

func (c *iec61851Controller) WriteCurrentLimit(evEntity spineapi.EntityRemoteInterface, current float64) error {
	span := tracer.StartSpan("iec61851.write_current_limit", tracer.Tag("current", current))
	defer span.Finish()

	// Check if overload protection is available
	if !c.opEV.IsScenarioAvailableAtEntity(evEntity, 1) {
		span.SetTag("error", "overload protection not available")
		return ErrOverloadProtectionUnavailable
	}

	// Get cached OPEV limits from provider (avoids deadlock from calling CurrentLimits in event handler)
	_, maxLimits := c.limitsProvider.GetOPEVLimits()

	// Setup obligation limits for all phases
	var limits []ucapi.LoadLimitsPhase
	for phase := 0; phase < c.chargingCfg.Phases; phase++ {
		limit := ucapi.LoadLimitsPhase{
			Phase:    ucapi.PhaseNameMapping[phase],
			IsActive: true,
			Value:    current,
		}

		// If limit equals or exceeds max, the limit is inactive (no restriction)
		if maxLimits != nil && phase < len(maxLimits) && current >= maxLimits[phase] {
			limit.IsActive = false
		}

		limits = append(limits, limit)
	}

	// Write the overload protection limits (obligations)
	_, err := c.opEV.WriteLoadControlLimits(evEntity, limits, nil)
	if err != nil {
		span.SetTag("error", err.Error())
		return err
	}

	c.logger.Debug("IEC 61851 obligation limits written",
		zap.Float64("current", current),
		zap.Int("phases", len(limits)),
	)

	return nil
}
