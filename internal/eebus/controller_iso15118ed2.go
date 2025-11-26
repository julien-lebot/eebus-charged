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
	chargingCfg *config.ChargingConfig
	evCC        ucapi.CemEVCCInterface
	opEV        ucapi.CemOPEVInterface
	oscEV       ucapi.CemOSCEVInterface
	logger      *zap.Logger
}

func newIso15118ed2Controller(
	chargingCfg *config.ChargingConfig,
	evCC ucapi.CemEVCCInterface,
	opEV ucapi.CemOPEVInterface,
	oscEV ucapi.CemOSCEVInterface,
	logger *zap.Logger,
) *iso15118ed2Controller {
	return &iso15118ed2Controller{
		chargingCfg: chargingCfg,
		evCC:        evCC,
		opEV:        opEV,
		oscEV:       oscEV,
		logger:      logger.With(zap.String("controller", "iso15118-2ed2")),
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

	// Try VAS/OSCEV mode first (VW/Porsche vehicles)
	if c.supportsVAS(evEntity) {
		c.logger.Debug("Using VAS/OSCEV mode for charging control", zap.Float64("current", current))
		return c.writeVASLimits(evEntity, current)
	}

	// Fallback to standard OPEV obligations
	c.logger.Debug("VAS not supported, using standard OPEV obligations", zap.Float64("current", current))
	return c.writeOPEVLimits(evEntity, current)
}

// supportsVAS checks if the vehicle supports VW VAS (PV mode)
func (c *iso15118ed2Controller) supportsVAS(evEntity spineapi.EntityRemoteInterface) bool {
	// Must use ISO 15118-2 (already validated by controller selection)
	
	// First check if vehicle announces OSCEV support in use cases list
	// This is the primary indicator - the vehicle announces what it supports
	useCases := evEntity.Device().UseCases()
	c.logger.Debug("VAS detection: Checking use cases",
		zap.Int("use_case_count", len(useCases)))
	
	oscevAnnounced := false
	for i, uci := range useCases {
		// Check entity address matches
		entityMatch := true
		if uci.Address != nil &&
			evEntity.Address() != nil &&
			slices.Compare(uci.Address.Entity, evEntity.Address().Entity) != 0 {
			entityMatch = false
			c.logger.Debug("VAS detection: Skipping use case for different entity address",
				zap.Int("index", i))
			continue
		}

		c.logger.Debug("VAS detection: Checking use case info",
			zap.Int("index", i),
			zap.Bool("entity_match", entityMatch),
			zap.Int("support_count", len(uci.UseCaseSupport)))

		for j, uc := range uci.UseCaseSupport {
			useCaseName := ""
			if uc.UseCaseName != nil {
				useCaseName = string(*uc.UseCaseName)
			}
			available := uc.UseCaseAvailable != nil && *uc.UseCaseAvailable
			
			c.logger.Debug("VAS detection: Use case",
				zap.Int("uc_index", j),
				zap.String("name", useCaseName),
				zap.Bool("available", available))
			
			if uc.UseCaseName != nil &&
				*uc.UseCaseName == model.UseCaseNameTypeOptimizationOfSelfConsumptionDuringEVCharging {
				c.logger.Info("VAS detection: OSCEV use case found in vehicle announcements",
					zap.Bool("available", available),
					zap.Bool("has_availability_flag", uc.UseCaseAvailable != nil))
				
				if available {
					oscevAnnounced = true
					c.logger.Debug("VAS detection: OSCEV use case found and available in use cases list")
				} else {
					c.logger.Warn("VAS detection: OSCEV use case found but marked as unavailable - vehicle may require certain conditions (e.g., charging active, SoC available, etc.)")
					// Note: Some vehicles announce OSCEV but mark it as unavailable until certain conditions are met
					// We'll still check scenario availability below, but won't use OSCEV if UseCaseAvailable is false
				}
				break
			}
		}
		if oscevAnnounced {
			break
		}
	}

	if !oscevAnnounced {
		c.logger.Info("VAS not detected: OSCEV use case not announced by vehicle - falling back to OPEV obligations")
		return false
	}

	// Secondary check: verify OSCEV scenario is available
	// This may not be available immediately after protocol upgrade, but if the vehicle
	// announces OSCEV support, we should try to use it anyway
	oscEVAvailable := c.oscEV.IsScenarioAvailableAtEntity(evEntity, 1)
	c.logger.Debug("VAS detection: OSCEV scenario check",
		zap.Bool("oscEV_scenario_available", oscEVAvailable))
	
	if !oscEVAvailable {
		c.logger.Warn("VAS detection: OSCEV announced by vehicle but scenario not yet available - will try anyway (scenario may bind later)")
		// Don't return false - if vehicle announces it, we should try to use it
		// The scenario binding may happen later
	}

	c.logger.Info("VAS detected: OSCEV use case announced by vehicle - using recommendation mode")
	return true
}

// writeVASLimits writes limits using VAS/OSCEV (recommendations)
func (c *iso15118ed2Controller) writeVASLimits(evEntity spineapi.EntityRemoteInterface, current float64) error {
	span := tracer.StartSpan("iso15118ed2.write_vas_limits", tracer.Tag("current", current))
	defer span.Finish()

	// Get min limits to determine IsActive flag
	minLimits, _, _, err := c.oscEV.CurrentLimits(evEntity)
	if err != nil {
		c.logger.Debug("Could not get OSCEV current limits", zap.Error(err))
		minLimits = nil
	}

	// Setup recommendation limits for all phases
	var limits []ucapi.LoadLimitsPhase
	for phase := 0; phase < c.chargingCfg.Phases; phase++ {
		limit := ucapi.LoadLimitsPhase{
			Phase: ucapi.PhaseNameMapping[phase],
			Value: current,
		}

		// IsActive logic for OSCEV:
		// - If value >= minLimit: IsActive=true (recommendation applies, vehicle charges)
		// - If value < minLimit: IsActive=false (recommendation doesn't apply, vehicle pauses)
		limit.IsActive = false
		if minLimits != nil && phase < len(minLimits) {
			limit.IsActive = current >= minLimits[phase]
		}

		limits = append(limits, limit)
	}

	// Write OSCEV recommendation limits
	if _, err := c.oscEV.WriteLoadControlLimits(evEntity, limits, nil); err != nil {
		span.SetTag("error", err.Error())
		return err
	}

	// Disable OPEV limits to avoid conflicts
	if err := c.disableOPEVLimits(evEntity); err != nil {
		c.logger.Warn("Failed to disable OPEV limits", zap.Error(err))
	}

	c.logger.Debug("VAS/OSCEV recommendation limits written",
		zap.Float64("current", current),
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

	// Get current limits from EVSE
	_, maxLimits, _, err := c.opEV.CurrentLimits(evEntity)
	if err != nil {
		c.logger.Debug("Could not get OPEV current limits", zap.Error(err))
		maxLimits = nil
	}

	// Setup obligation limits for all phases
	var limits []ucapi.LoadLimitsPhase
	for phase := 0; phase < c.chargingCfg.Phases; phase++ {
		limit := ucapi.LoadLimitsPhase{
			Phase:    ucapi.PhaseNameMapping[phase],
			IsActive: true,
			Value:    current,
		}

		// If limit equals or exceeds max, the limit is inactive
		if maxLimits != nil && phase < len(maxLimits) && current >= maxLimits[phase] {
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
	_, err = c.opEV.WriteLoadControlLimits(evEntity, limits, nil)
	if err != nil {
		span.SetTag("error", err.Error())
		return err
	}

	c.logger.Debug("ISO 15118-2 OPEV obligation limits written",
		zap.Float64("current", current),
		zap.Int("phases", len(limits)),
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

