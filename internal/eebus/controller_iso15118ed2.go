package eebus

import (
	"slices"

	ucapi "github.com/enbility/eebus-go/usecases/api"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"
	"github.com/julienar/eebus-charged/internal/config"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// iso15118ed2Controller handles charging via ISO 15118-2 Edition 2
// Supports VW VAS mode using OSCEV (recommendations) instead of OPEV (obligations)
type iso15118ed2Controller struct {
	chargingCfg    *config.ChargingConfig
	evCC           ucapi.CemEVCCInterface
	opEV           ucapi.CemOPEVInterface
	oscEV          ucapi.CemOSCEVInterface
	limitsProvider LimitsProvider
	logger         *zap.Logger
}

func newIso15118ed2Controller(
	chargingCfg *config.ChargingConfig,
	evCC ucapi.CemEVCCInterface,
	opEV ucapi.CemOPEVInterface,
	oscEV ucapi.CemOSCEVInterface,
	limitsProvider LimitsProvider,
	logger *zap.Logger,
) *iso15118ed2Controller {
	return &iso15118ed2Controller{
		chargingCfg:    chargingCfg,
		evCC:           evCC,
		opEV:           opEV,
		oscEV:          oscEV,
		limitsProvider: limitsProvider,
		logger:         logger.With(zap.String("controller", "iso15118-2ed2")),
	}
}

func (c *iso15118ed2Controller) Name() string {
	return "iso15118-2ed2"
}

func (c *iso15118ed2Controller) GetCapabilities(evEntity spineapi.EntityRemoteInterface) ControllerCapabilities {
	vasSupported := false
	oscevAvailable := false
	opevAvailable := false

	if evEntity != nil {
		vasSupported = c.supportsVAS(evEntity)
		oscevAvailable = c.oscEV.IsScenarioAvailableAtEntity(evEntity, 1)
		opevAvailable = c.opEV.IsScenarioAvailableAtEntity(evEntity, 1)
	}

	return ControllerCapabilities{
		CommunicationStandard: "iso15118-2ed2",
		ControllerType:        "iso15118-2ed2",
		VASSupported:          &vasSupported,
		OSCEVAvailable:        &oscevAvailable,
		OPEVAvailable:         &opevAvailable,
	}
}

func (c *iso15118ed2Controller) WriteCurrentLimit(evEntity spineapi.EntityRemoteInterface, current float64) error {
	span := tracer.StartSpan("iso15118ed2.write_current_limit", tracer.Tag("current", current))
	defer span.Finish()

	// Hierarchy: Try VAS/OSCEV first, then OSCEV standalone, then OPEV as last resort

	// 1. Try VAS/OSCEV mode (VW/Porsche vehicles with full VAS support)
	if c.supportsVAS(evEntity) {
		c.logger.Debug("Using VAS/OSCEV mode for charging control", zap.Float64("current", current))
		return c.writeOSCEVLimits(evEntity, current)
	}

	// 2. Try OSCEV standalone (even without VAS, if available)
	if c.oscEV.IsScenarioAvailableAtEntity(evEntity, 1) {
		c.logger.Debug("VAS not supported, trying OSCEV standalone", zap.Float64("current", current))
		return c.writeOSCEVLimits(evEntity, current)
	}

	// 3. Fallback to standard OPEV obligations
	c.logger.Debug("OSCEV not available, using standard OPEV obligations", zap.Float64("current", current))
	return c.writeOPEVLimits(evEntity, current)
}

// supportsVAS checks if the vehicle supports VW VAS (PV mode)
// Caches result to avoid repeated checks and excessive logging
func (c *iso15118ed2Controller) supportsVAS(evEntity spineapi.EntityRemoteInterface) bool {
	// Must use ISO 15118-2 (already validated by controller selection)

	// First check if vehicle announces OSCEV support in use cases list
	// This is the primary indicator - the vehicle announces what it supports
	useCases := evEntity.Device().UseCases()

	oscevAnnounced := false
	oscevAvailable := false
	for _, uci := range useCases {
		// Check entity address matches
		if uci.Address != nil &&
			evEntity.Address() != nil &&
			slices.Compare(uci.Address.Entity, evEntity.Address().Entity) != 0 {
			continue
		}

		for _, uc := range uci.UseCaseSupport {
			if uc.UseCaseName != nil &&
				*uc.UseCaseName == model.UseCaseNameTypeOptimizationOfSelfConsumptionDuringEVCharging {
				available := uc.UseCaseAvailable != nil && *uc.UseCaseAvailable
				oscevAnnounced = true
				oscevAvailable = available
				break
			}
		}
		if oscevAnnounced {
			break
		}
	}

	if !oscevAnnounced {
		// Only log once at debug level to avoid spam
		c.logger.Debug("VAS not detected: OSCEV use case not announced by vehicle")
		return false
	}

	// Secondary check: verify OSCEV scenario is available
	oscEVScenarioAvailable := c.oscEV.IsScenarioAvailableAtEntity(evEntity, 1)

	// VAS is supported if OSCEV is announced AND (available OR scenario is available)
	// Some vehicles announce it but mark as unavailable until conditions are met
	if oscevAvailable || oscEVScenarioAvailable {
		c.logger.Info("VAS detected: OSCEV use case announced by vehicle - using recommendation mode",
			zap.Bool("use_case_available", oscevAvailable),
			zap.Bool("scenario_available", oscEVScenarioAvailable))
		return true
	}

	// OSCEV announced but not yet available - log once at debug level
	c.logger.Debug("VAS: OSCEV announced but not yet available - may become available when charging starts")
	return false
}

// writeOSCEVLimits writes limits using OSCEV (recommendations)
// This is used both for VAS mode and standalone OSCEV
func (c *iso15118ed2Controller) writeOSCEVLimits(evEntity spineapi.EntityRemoteInterface, current float64) error {
	span := tracer.StartSpan("iso15118ed2.write_oscev_limits", tracer.Tag("current", current))
	defer span.Finish()

	// Get cached OSCEV limits from provider (avoids deadlock from calling CurrentLimits in event handler)
	minLimits, maxLimits := c.limitsProvider.GetOSCEVLimits()

	// For OSCEV in ISO mode, current=0 may cause error state in some vehicles (e.g., Porsche Macan)
	// Use minimum current instead of 0 to avoid error state while still effectively pausing
	effectiveCurrent := current
	if current == 0 {
		if minLimits != nil && len(minLimits) > 0 {
			// Find the minimum across all phases
			minCurrent := minLimits[0]
			for _, min := range minLimits {
				if min < minCurrent {
					minCurrent = min
				}
			}
			if minCurrent > 0 {
				effectiveCurrent = minCurrent
				c.logger.Debug("Using minimum current instead of 0 for OSCEV stop to avoid error state",
					zap.Float64("min_current", effectiveCurrent))
			}
		}
		// If no min limits available or min is 0, keep effectiveCurrent as 0
		// (will use 0, which may cause error state, but we have no better option)
	}

	// Setup recommendation limits for all phases
	var limits []ucapi.LoadLimitsPhase
	for phase := 0; phase < c.chargingCfg.Phases; phase++ {
		limit := ucapi.LoadLimitsPhase{
			Phase:    ucapi.PhaseNameMapping[phase],
			IsActive: true,
			Value:    effectiveCurrent,
		}

		// According to EEBUS spec: IsActive=true means "apply the limit", IsActive=false means "ignore the limit"
		// If Value >= maxLimit, set IsActive=false to indicate "no constraint" (similar to OPEV behavior)
		// If Value < maxLimit, IsActive=true means "apply this recommendation"
		if maxLimits != nil && phase < len(maxLimits) && effectiveCurrent >= maxLimits[phase] {
			limit.IsActive = false
		}

		limits = append(limits, limit)
	}

	// Disable OPEV limits to avoid conflicts
	if err := c.disableOPEVLimits(evEntity); err != nil {
		c.logger.Warn("Failed to disable OPEV limits", zap.Error(err))
	}

	// Write OSCEV recommendation limits
	if _, err := c.oscEV.WriteLoadControlLimits(evEntity, limits, nil); err != nil {
		span.SetTag("error", err.Error())
		return err
	}

	c.logger.Debug("OSCEV recommendation limits written",
		zap.Float64("current", effectiveCurrent),
		zap.Float64("original_current", current),
		zap.Int("phases", len(limits)),
		zap.Bool("active", limits[0].IsActive),
	)

	return nil
}

// writeOPEVLimits writes limits using standard OPEV (obligations)
func (c *iso15118ed2Controller) writeOPEVLimits(evEntity spineapi.EntityRemoteInterface, current float64) error {
	span := tracer.StartSpan("iso15118ed2.write_opev_limits", tracer.Tag("current", current))
	defer span.Finish()

	// Check if overload protection is available
	if !c.opEV.IsScenarioAvailableAtEntity(evEntity, 1) {
		span.SetTag("error", "overload protection not available")
		return ErrOverloadProtectionUnavailable
	}

	// Get cached OPEV limits from provider (avoids deadlock from calling CurrentLimits in event handler)
	minLimits, maxLimits := c.limitsProvider.GetOPEVLimits()

	// For OPEV in ISO mode, current=0 may cause error state in some vehicles (e.g., Porsche Macan)
	// Use minimum current instead of 0 to avoid error state while still effectively pausing
	effectiveCurrent := current
	if current == 0 {
		if minLimits != nil && len(minLimits) > 0 {
			// Find the minimum across all phases
			minCurrent := minLimits[0]
			for _, min := range minLimits {
				if min < minCurrent {
					minCurrent = min
				}
			}
			if minCurrent > 0 {
				effectiveCurrent = minCurrent
				c.logger.Debug("Using minimum current instead of 0 for OPEV stop to avoid error state",
					zap.Float64("min_current", effectiveCurrent))
			}
		}
		// If no min limits available or min is 0, keep effectiveCurrent as 0
		// (will use 0, which may cause error state, but we have no better option)
	}

	// Setup obligation limits for all phases
	var limits []ucapi.LoadLimitsPhase
	for phase := 0; phase < c.chargingCfg.Phases; phase++ {
		limit := ucapi.LoadLimitsPhase{
			Phase:    ucapi.PhaseNameMapping[phase],
			IsActive: true,
			Value:    effectiveCurrent,
		}

		// If limit equals or exceeds max, the limit is inactive
		if maxLimits != nil && phase < len(maxLimits) && effectiveCurrent >= maxLimits[phase] {
			limit.IsActive = false
		}

		limits = append(limits, limit)
	}

	// Disable OSCEV if it exists
	if c.oscEV.IsScenarioAvailableAtEntity(evEntity, 1) {
		if _, err := c.oscEV.LoadControlLimits(evEntity); err == nil {
			if err := c.disableOSCEVLimits(evEntity); err != nil {
				c.logger.Warn("Failed to disable OSCEV limits", zap.Error(err))
			}
		}
	}

	// Write OPEV obligation limits
	_, err := c.opEV.WriteLoadControlLimits(evEntity, limits, nil)
	if err != nil {
		span.SetTag("error", err.Error())
		return err
	}

	c.logger.Debug("ISO 15118-2 OPEV obligation limits written",
		zap.Float64("current", effectiveCurrent),
		zap.Float64("original_current", current),
		zap.Int("phases", len(limits)),
		zap.Bool("active", limits[0].IsActive),
	)

	return nil
}

// disableOPEVLimits disables all OPEV limits
func (c *iso15118ed2Controller) disableOPEVLimits(evEntity spineapi.EntityRemoteInterface) error {
	limits, err := c.opEV.LoadControlLimits(evEntity)
	if err != nil {
		return err
	}

	var writeNeeded bool
	for index, limit := range limits {
		if limit.IsActive {
			limits[index].IsActive = false
			writeNeeded = true
		}
	}

	if writeNeeded {
		_, err = c.opEV.WriteLoadControlLimits(evEntity, limits, nil)
	}

	return err
}

// disableOSCEVLimits disables all OSCEV limits
func (c *iso15118ed2Controller) disableOSCEVLimits(evEntity spineapi.EntityRemoteInterface) error {
	limits, err := c.oscEV.LoadControlLimits(evEntity)
	if err != nil {
		return err
	}

	var writeNeeded bool
	for index, limit := range limits {
		if limit.IsActive {
			limits[index].IsActive = false
			writeNeeded = true
		}
	}

	if writeNeeded {
		_, err = c.oscEV.WriteLoadControlLimits(evEntity, limits, nil)
	}

	return err
}

// disableLimitsGeneric is a generic helper for disabling limits
func (c *iso15118ed2Controller) disableLimitsGeneric(
	evEntity spineapi.EntityRemoteInterface,
	getLimits func(spineapi.EntityRemoteInterface) ([]ucapi.LoadLimitsPhase, error),
	writeLimits func(spineapi.EntityRemoteInterface, []ucapi.LoadLimitsPhase, func(result any)) (*uint64, error),
) error {
	limits, err := getLimits(evEntity)
	if err != nil {
		return err
	}

	var writeNeeded bool
	for index, limit := range limits {
		if limit.IsActive {
			limits[index].IsActive = false
			writeNeeded = true
		}
	}

	if writeNeeded {
		_, err = writeLimits(evEntity, limits, nil)
	}

	return err
}
