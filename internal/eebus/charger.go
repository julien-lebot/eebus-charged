package eebus

import (
	"fmt"
	"sync"

	eebusapi "github.com/enbility/eebus-go/api"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/cem/evcc"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/julienar/eebus-charged/internal/config"
	"github.com/julienar/eebus-charged/internal/mqtt"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

const (
	voltage = 230.0 // Voltage per phase
)

// ChargingState represents the current charging state
type ChargingState string

const (
	ChargingStateStopped ChargingState = "stopped"
	ChargingStateActive  ChargingState = "active"
	ChargingStateUnknown ChargingState = "unknown"
)

// Charger represents an EEBUS-compatible EV charger
type Charger struct {
	config      config.ChargerConfig
	chargingCfg *config.ChargingConfig
	device      spineapi.DeviceRemoteInterface
	logger      *zap.Logger
	mqttHandler *mqtt.MqttHandler

	// EEBUS use cases
	evCC  ucapi.CemEVCCInterface
	evCem ucapi.CemEVCEMInterface
	evSoc ucapi.CemEVSOCInterface
	opEV  ucapi.CemOPEVInterface
	oscEV ucapi.CemOSCEVInterface

	mu            sync.RWMutex
	evEntity      spineapi.EntityRemoteInterface
	currentLimit  float64
	vehicleID     string
	isConnected   bool
	chargingState ChargingState
}

// NewCharger creates a new charger instance
func NewCharger(
	cfg config.ChargerConfig,
	device spineapi.DeviceRemoteInterface,
	logger *zap.Logger,
	chargingCfg *config.ChargingConfig,
	evCC ucapi.CemEVCCInterface,
	evCem ucapi.CemEVCEMInterface,
	evSoc ucapi.CemEVSOCInterface,
	opEV ucapi.CemOPEVInterface,
	oscEV ucapi.CemOSCEVInterface,
	mqttHandler *mqtt.MqttHandler,
) *Charger {
	return &Charger{
		config:        cfg,
		chargingCfg:   chargingCfg,
		device:        device,
		logger:        logger.With(zap.String("charger", cfg.Name)),
		mqttHandler:  mqttHandler,
		evCC:          evCC,
		evCem:         evCem,
		evSoc:         evSoc,
		opEV:          opEV,
		oscEV:         oscEV,
		currentLimit:  chargingCfg.DefaultCurrent,
		chargingState: ChargingStateUnknown,
	}
}

// Name returns the charger name
func (c *Charger) Name() string {
	return c.config.Name
}

// SKI returns the charger's SKI
func (c *Charger) SKI() string {
	return c.device.Ski()
}

// IsConnected returns whether a vehicle is connected
func (c *Charger) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isConnected
}

// VehicleID returns the connected vehicle's ID
func (c *Charger) VehicleID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.vehicleID
}

// ChargingState returns the current charging state
func (c *Charger) ChargingState() ChargingState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.chargingState
}

// CurrentLimit returns the current charging limit in Amperes
func (c *Charger) CurrentLimit() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentLimit
}

// StartCharging starts the charging process
func (c *Charger) StartCharging() error {
	span := tracer.StartSpan("charger.start_charging", tracer.Tag("charger", c.config.Name))
	defer span.Finish()

	c.logger.Info("Starting charging")

	c.mu.RLock()
	evEntity := c.evEntity
	current := c.currentLimit
	c.mu.RUnlock()

	if evEntity == nil {
		return fmt.Errorf("no vehicle connected")
	}

	if err := c.writeCurrentLimit(evEntity, current); err != nil {
		span.SetTag("error", err)
		return fmt.Errorf("failed to start charging: %w", err)
	}

	c.mu.Lock()
	c.chargingState = ChargingStateActive
	c.mu.Unlock()

	c.logger.Info("Charging started successfully", zap.Float64("current", current))
	c.publishState()
	return nil
}

// StopCharging stops the charging process
func (c *Charger) StopCharging() error {
	span := tracer.StartSpan("charger.stop_charging", tracer.Tag("charger", c.config.Name))
	defer span.Finish()

	c.logger.Info("Stopping charging")

	c.mu.RLock()
	evEntity := c.evEntity
	c.mu.RUnlock()

	if evEntity == nil {
		return fmt.Errorf("no vehicle connected")
	}

	// Set current to 0 to stop charging
	if err := c.writeCurrentLimit(evEntity, 0); err != nil {
		span.SetTag("error", err)
		return fmt.Errorf("failed to stop charging: %w", err)
	}

	c.mu.Lock()
	c.chargingState = ChargingStateStopped
	c.mu.Unlock()

	c.logger.Info("Charging stopped successfully")
	c.publishState()
	return nil
}


// SetCurrentLimit sets the maximum charging current in Amperes
func (c *Charger) SetCurrentLimit(current float64) error {
	span := tracer.StartSpan("charger.set_current_limit",
		tracer.Tag("charger", c.config.Name),
		tracer.Tag("current", current))
	defer span.Finish()

	// Validate current is positive
	if current < 0 {
		return fmt.Errorf("current must be positive")
	}

	c.mu.RLock()
	evEntity := c.evEntity
	c.mu.RUnlock()

	// Get EV limits if vehicle is connected
	var evMinAmps, evMaxAmps float64
	if evEntity != nil {
		minWatts, maxWatts, _, err := c.evCC.ChargingPowerLimits(evEntity)
		if err == nil {
			// Convert watts to amps (230V per phase * N phases)
			evMinAmps = minWatts / (230.0 * float64(c.chargingCfg.Phases))
			evMaxAmps = maxWatts / (230.0 * float64(c.chargingCfg.Phases))
			c.logger.Debug("EV current limits",
				zap.Float64("ev_min_amps", evMinAmps),
				zap.Float64("ev_max_amps", evMaxAmps))
		}
	}

	// Determine effective min/max (most restrictive of EV limits and config limits)
	effectiveMin := c.chargingCfg.MinCurrent
	effectiveMax := c.chargingCfg.MaxCurrent
	if evEntity != nil && evMinAmps > 0 && evMinAmps > effectiveMin {
		effectiveMin = evMinAmps
		c.logger.Debug("Using EV minimum", zap.Float64("ev_min", evMinAmps))
	}
	if evEntity != nil && evMaxAmps > 0 && evMaxAmps < effectiveMax {
		effectiveMax = evMaxAmps
		c.logger.Debug("Using EV maximum", zap.Float64("ev_max", evMaxAmps))
	}

	// Validate and clamp to effective limits
	if current > 0 && current < effectiveMin {
		c.logger.Warn("Requested current below minimum, using minimum",
			zap.Float64("requested", current),
			zap.Float64("minimum", effectiveMin),
			zap.Bool("ev_connected", evEntity != nil))
		current = effectiveMin
	}

	if current > effectiveMax {
		c.logger.Warn("Requested current above maximum, using maximum",
			zap.Float64("requested", current),
			zap.Float64("maximum", effectiveMax),
			zap.Bool("ev_connected", evEntity != nil))
		current = effectiveMax
	}

	c.mu.Lock()
	c.currentLimit = current
	c.mu.Unlock()

	// If a vehicle is connected, update the limit immediately
	if evEntity != nil && c.chargingState == ChargingStateActive {
		if err := c.writeCurrentLimit(evEntity, current); err != nil {
			return fmt.Errorf("failed to update current limit: %w", err)
		}
	}

	c.logger.Info("Current limit updated", zap.Float64("current", current))
	c.publishState()
	return nil
}

// GetStatus returns detailed status information
func (c *Charger) GetStatus() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := map[string]interface{}{
		"name":           c.config.Name,
		"ski":            c.device.Ski(),
		"connected":      c.isConnected,
		"charging_state": string(c.chargingState),
		"current_limit":  c.currentLimit,
		"min_current":    c.chargingCfg.MinCurrent,
		"max_current":    c.chargingCfg.MaxCurrent,
	}

	if c.isConnected {
		status["vehicle_id"] = c.vehicleID
	}

	return status
}

// writeCurrentLimit writes the current limit to the EV via EEBUS
func (c *Charger) writeCurrentLimit(evEntity spineapi.EntityRemoteInterface, current float64) error {
	// Check if overload protection is available
	if !c.opEV.IsScenarioAvailableAtEntity(evEntity, 1) {
		return fmt.Errorf("overload protection not available")
	}

	// Get current limits from EVSE to know what's possible
	_, maxLimits, _, err := c.opEV.CurrentLimits(evEntity)
	if err != nil {
		c.logger.Warn("Could not get current limits from EVSE", zap.Error(err))
	}

	// Setup the limit data for all phases
	var limits []ucapi.LoadLimitsPhase
	for phase := 0; phase < c.chargingCfg.Phases; phase++ {
		limit := ucapi.LoadLimitsPhase{
			Phase:    ucapi.PhaseNameMapping[phase],
			IsActive: true,
			Value:    current,
		}

		// If the limit equals or exceeds the max allowed, the limit is inactive
		if phase < len(maxLimits) && current >= maxLimits[phase] {
			limit.IsActive = false
		}

		limits = append(limits, limit)
	}

	// Disable optimization of self consumption limits if they exist
	if c.oscEV.IsScenarioAvailableAtEntity(evEntity, 1) {
		if _, err := c.oscEV.LoadControlLimits(evEntity); err == nil {
			// Make sure the OSCEV limits are inactive
			if err := c.disableLimits(evEntity, c.oscEV); err != nil {
				c.logger.Warn("Failed to disable OSCEV limits", zap.Error(err))
			}
		}
	}

	// Write the overload protection limits
	_, err = c.opEV.WriteLoadControlLimits(evEntity, limits, nil)
	if err != nil {
		return fmt.Errorf("failed to write load control limits: %w", err)
	}

	c.logger.Debug("Current limit written to EV",
		zap.Float64("current", current),
		zap.Int("phases", len(limits)),
	)

	return nil
}

// disableLimits disables all limits for a given use case
func (c *Charger) disableLimits(evEntity spineapi.EntityRemoteInterface, uc interface{}) error {
	type limitController interface {
		LoadControlLimits(spineapi.EntityRemoteInterface) ([]ucapi.LoadLimitsPhase, error)
		WriteLoadControlLimits(spineapi.EntityRemoteInterface, []ucapi.LoadLimitsPhase, func(result any)) (*uint64, error)
	}

	controller, ok := uc.(limitController)
	if !ok {
		return fmt.Errorf("use case does not support load control limits")
	}

	limits, err := controller.LoadControlLimits(evEntity)
	if err != nil {
		return err
	}

	// Check if any limits are active
	var writeNeeded bool
	for index, limit := range limits {
		if limit.IsActive {
			limits[index].IsActive = false
			writeNeeded = true
		}
	}

	if writeNeeded {
		_, err = controller.WriteLoadControlLimits(evEntity, limits, nil)
	}

	return err
}

// handleUseCaseEvent handles EEBUS use case events
func (c *Charger) handleUseCaseEvent(device spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	c.mu.Lock()
	c.evEntity = entity
	wasConnected := c.isConnected
	c.mu.Unlock()

	switch event {
	case evcc.EvConnected:
		// EV connected - automatically stop charging to prevent unwanted charging
		c.mu.Lock()
		c.isConnected = true
		c.mu.Unlock()

		// Try to get vehicle identification
		if identifications, err := c.evCC.Identifications(entity); err == nil && len(identifications) > 0 {
			c.mu.Lock()
			c.vehicleID = identifications[0].Value
			c.mu.Unlock()
			c.logger.Info("Vehicle connected",
				zap.String("vehicle_id", c.vehicleID))
		} else {
			c.logger.Info("Vehicle connected (identification not available)")
		}

		// Log communication standard (important for understanding SoC availability)
		if commStd, err := c.evCC.CommunicationStandard(entity); err == nil {
			c.logger.Info("Vehicle communication standard",
				zap.String("standard", string(commStd)))
		}

		// Automatically stop charging when vehicle connects
		// User must explicitly call StartCharging() to begin charging
		// Note: c.evEntity is already set above, so StopCharging() will work
		if err := c.StopCharging(); err != nil {
			c.logger.Warn("Failed to stop charging on connect", zap.Error(err))
		} else {
			c.logger.Info("Vehicle connected - charging stopped (call start to begin)")
		}

	case evcc.EvDisconnected:
		// EV disconnected
		c.mu.Lock()
		c.logger.Info("Vehicle disconnected")
		c.evEntity = nil
		c.isConnected = false
		c.vehicleID = ""
		c.chargingState = ChargingStateUnknown
		c.mu.Unlock()
		c.publishState()

	default:
		// Other events like DataUpdateCurrentPerPhase
		if wasConnected {
			c.mu.Lock()
			c.updateChargingState()
			c.mu.Unlock()
			c.publishState()
		}
	}
}

// updateChargingState updates the charging state based on current measurements
func (c *Charger) updateChargingState() {
	if c.evEntity == nil {
		return
	}

	// Try to get the charge state from EVCC
	if chargeState, err := c.evCC.ChargeState(c.evEntity); err == nil {
		switch chargeState {
		case ucapi.EVChargeStateTypeActive:
			// Check if actually charging by looking at power
			if c.isActuallyCharging() {
				c.chargingState = ChargingStateActive
			} else {
				c.chargingState = ChargingStateStopped
			}
		case ucapi.EVChargeStateTypePaused, ucapi.EVChargeStateTypeFinished:
			c.chargingState = ChargingStateStopped
		default:
			c.chargingState = ChargingStateUnknown
		}
	}
}

// isActuallyCharging checks if the EV is actually drawing power
func (c *Charger) isActuallyCharging() bool {
	if c.evEntity == nil {
		return false
	}

	// Check if we can get current measurements
	if !c.evCem.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		return false
	}

	currents, err := c.evCem.CurrentPerPhase(c.evEntity)
	if err != nil || len(currents) == 0 {
		return false
	}

	// Consider charging if any phase has significant current (>1A)
	for _, current := range currents {
		if current > 1.0 {
			return true
		}
	}

	return false
}

// publishState publishes the current charger state to MQTT
func (c *Charger) publishState() {
	if c.mqttHandler == nil {
		return
	}

	span := tracer.StartSpan("charger.publish_state", tracer.Tag("charger", c.config.Name))
	defer span.Finish()

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Get current power
	var chargePower float64
	if c.evEntity != nil && c.evCem.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		if currents, err := c.evCem.CurrentPerPhase(c.evEntity); err == nil {
			// Calculate total power (sum of all phases)
			for _, current := range currents {
				chargePower += current * voltage // W = A * V
			}
		}
	}

	// Try to get vehicle SoC (only works with ISO 15118-20 or ISO 15118-2 with VAS)
	var vehicleSoC *float64
	if c.evEntity != nil && c.evSoc.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		if soc, err := c.evSoc.StateOfCharge(c.evEntity); err == nil {
			vehicleSoC = &soc
			c.logger.Info("Vehicle SoC available", zap.Float64("soc", soc))
		}
	}

	state := &mqtt.ChargerState{
		VehicleConnected:  c.isConnected,
		VehicleIdentity:   c.vehicleID,
		VehicleName:       c.vehicleID, // EEBUS doesn't provide a separate name
		Charging:          c.chargingState == ChargingStateActive && chargePower > 100,
		ChargingState:     string(c.chargingState),
		ChargePower:       chargePower,
		CurrentLimit:      c.currentLimit,
		SessionEnergy:     0, // ISO 15118-2 doesn't provide session energy
		VehicleSoC:        vehicleSoC, // Available with ISO 15118-20 or ISO 15118-2 with VAS
		ChargeRemainingEnergy: nil, // Not available from ISO 15118
	}

	if err := c.mqttHandler.PublishChargerState(c.config.Name, state); err != nil {
		c.logger.Warn("Failed to publish MQTT state", zap.Error(err))
	}
}

// MonitorCharging can be called periodically to update state
func (c *Charger) MonitorCharging() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.evEntity == nil {
		return
	}

	c.updateChargingState()
}

