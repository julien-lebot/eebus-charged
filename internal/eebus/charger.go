package eebus

import (
	"fmt"
	"sync"

	eebusapi "github.com/enbility/eebus-go/api"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/cem/evcc"
	"github.com/enbility/eebus-go/usecases/cem/evcem"
	"github.com/enbility/eebus-go/usecases/cem/evsoc"
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
	vehicleNames map[string]string // Optional: map vehicle_id -> friendly_name

	// EEBUS use cases
	evCC  ucapi.CemEVCCInterface
	evCem ucapi.CemEVCEMInterface
	evSoc ucapi.CemEVSOCInterface
	opEV  ucapi.CemOPEVInterface
	oscEV ucapi.CemOSCEVInterface

	mu                    sync.RWMutex
	evEntity              spineapi.EntityRemoteInterface
	currentLimit          float64
	vehicleID             string
	isConnected           bool
	chargingState         ChargingState
	communicationStandard string                     // ISO 15118-2, ISO 15118-20, etc.
	controller            ChargingController         // Created when first charge state received (protocol ready)
	manufacturerData      *eebusapi.ManufacturerData // Vehicle manufacturer info
	sessionEnergy         float64                    // Energy charged in kWh
	currentPerPhase       []float64                  // Current per phase in A (L1, L2, L3)
	powerPerPhase         []float64                  // Power per phase in W (L1, L2, L3)
	vehicleSoC            *float64                   // Vehicle State of Charge (%)
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
	vehicleNames map[string]string,
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
		vehicleNames:  vehicleNames,
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

	c.logger.Debug("Current limit updated", zap.Float64("current", current))
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
// Delegates to the communication-standard-specific controller
func (c *Charger) writeCurrentLimit(evEntity spineapi.EntityRemoteInterface, current float64) error {
	span := tracer.StartSpan("charger.write_current_limit",
		tracer.Tag("charger", c.config.Name),
		tracer.Tag("current", current))
	defer span.Finish()

	// Controller is created when communication standard is received
	// If controller doesn't exist yet, protocol is still negotiating
	if c.controller == nil {
		span.SetTag("deferred", true)
		c.logger.Info("Protocol negotiation in progress, deferring limit write",
			zap.Float64("current", current),
			zap.String("standard", c.communicationStandard))
		return fmt.Errorf("protocol negotiation in progress - controller will be created when standard is known")
	}

	// Delegate to the controller
	err := c.controller.WriteCurrentLimit(evEntity, current)
	if err != nil {
		span.SetTag("error", err.Error())
		return err
	}

	c.logger.Debug("Current limit written via controller",
		zap.Float64("current", current),
		zap.String("controller", c.controller.Name()),
	)

	return nil
}


// handleUseCaseEvent handles EEBUS use case events
func (c *Charger) handleUseCaseEvent(device spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	span := tracer.StartSpan("charger.handle_use_case_event",
		tracer.Tag("charger", c.config.Name),
		tracer.Tag("event", string(event)))
	defer span.Finish()

	c.mu.Lock()
	c.evEntity = entity

	switch event {
	case evcc.EvConnected:
		// Vehicle connected - just set the flag, data will come via other events
		c.logger.Info("Vehicle connected")
		c.isConnected = true

	case evcc.EvDisconnected:
		// Vehicle disconnected - clear all state
		c.logger.Info("Vehicle disconnected")
		c.evEntity = nil
		c.isConnected = false
		c.vehicleID = ""
		c.chargingState = ChargingStateUnknown
		c.communicationStandard = ""
		c.controller = nil
		c.manufacturerData = nil
		c.sessionEnergy = 0
		c.currentPerPhase = nil
		c.powerPerPhase = nil
		c.vehicleSoC = nil

	case evcc.DataUpdateIdentifications:
		// Vehicle ID updated
		c.updateVehicleIdentification()

	case evcc.DataUpdateCommunicationStandard:
		// Communication standard updated (IEC 61851, ISO 15118-2, ISO 15118-20)
		c.updateCommunicationStandard()

	case evcc.DataUpdateManufacturerData:
		// Vehicle manufacturer data updated
		c.updateManufacturerData()

	case evcc.DataUpdateChargeState:
		// Charging state changed (active, paused, finished)
		c.updateChargeState()

	case evcem.DataUpdateCurrentPerPhase:
		// Current measurements updated
		c.updateCurrentPerPhase()

	case evcem.DataUpdatePowerPerPhase:
		// Power measurements updated
		c.updatePowerPerPhase()

	case evcem.DataUpdateEnergyCharged:
		// Energy charged updated
		c.updateSessionEnergy()

	case evsoc.DataUpdateStateOfCharge:
		// Vehicle SoC updated (ISO 15118-20 or ISO 15118-2 with VAS)
		c.updateStateOfCharge()

	case evcc.UseCaseSupportUpdate, evcem.UseCaseSupportUpdate, evsoc.UseCaseSupportUpdate:
		// Use case support changed - log for debugging
		c.logger.Debug("Use case support updated", zap.String("event", string(event)))

	default:
		// Unknown event - log for debugging
		c.logger.Debug("Unhandled use case event", zap.String("event", string(event)))
	}

	c.mu.Unlock()
	c.publishState()
}

// updateVehicleIdentification updates the vehicle ID when it changes
func (c *Charger) updateVehicleIdentification() {
	if c.evEntity == nil {
		return
	}

	identifications, err := c.evCC.Identifications(c.evEntity)
	if err != nil {
		return
	}

	newID := identifications[0].Value
	if newID != "" && newID != c.vehicleID {
		c.vehicleID = newID
		c.logger.Info("Vehicle identification received",
			zap.String("vehicle_id", c.vehicleID))
	}
}

// updateCommunicationStandard updates the communication standard when it changes or upgrades
func (c *Charger) updateCommunicationStandard() {
	if c.evEntity == nil {
		return
	}

	commStd, err := c.evCC.CommunicationStandard(c.evEntity)
	if err != nil {
		return
	}

	newStd := string(commStd)
	if newStd != "" && newStd != c.communicationStandard {
		oldStd := c.communicationStandard
		c.communicationStandard = newStd

		// Create controller when communication standard is known
		// Don't wait for charge state - protocol is ready when standard is received
		if c.controller == nil && c.evEntity != nil {
			c.controller = createController(
				c.communicationStandard,
				c.chargingCfg,
				c.evCC,
				c.opEV,
				c.oscEV,
				c.logger,
			)
			c.logger.Info("Controller created for communication standard",
				zap.String("standard", c.communicationStandard),
				zap.String("controller", c.controller.Name()))
		}

		if oldStd == "" {
			c.logger.Info("Vehicle communication standard received",
				zap.String("standard", c.communicationStandard))
		} else {
			c.logger.Info("Vehicle communication standard upgraded",
				zap.String("old_standard", oldStd),
				zap.String("new_standard", c.communicationStandard))
			
			// Recreate controller for upgraded protocol
			// This handles the transition from IEC61851 to ISO15118-2
			if c.controller != nil {
				c.controller = createController(
					c.communicationStandard,
					c.chargingCfg,
					c.evCC,
					c.opEV,
					c.oscEV,
					c.logger,
				)
				c.logger.Info("Controller recreated for upgraded protocol",
					zap.String("controller", c.controller.Name()))
				
				// Re-apply current limits with new controller
				if c.evEntity != nil {
					c.logger.Info("Re-applying limits with upgraded controller",
						zap.Float64("current", c.currentLimit))
					
					if err := c.writeCurrentLimit(c.evEntity, c.currentLimit); err != nil {
						c.logger.Warn("Failed to re-apply limits after protocol upgrade", zap.Error(err))
					}
				}
			}
		}
	}
}

// updateManufacturerData updates vehicle manufacturer information
func (c *Charger) updateManufacturerData() {
	if c.evEntity == nil {
		return
	}

	mfgData, err := c.evCC.ManufacturerData(c.evEntity)
	if err != nil {
		c.logger.Debug("Could not read manufacturer data", zap.Error(err))
		return
	}

	c.manufacturerData = &mfgData

	// Log interesting fields if available
	fields := []zap.Field{}
	if mfgData.DeviceName != "" {
		fields = append(fields, zap.String("device_name", mfgData.DeviceName))
	}
	if mfgData.BrandName != "" {
		fields = append(fields, zap.String("brand", mfgData.BrandName))
	}
	if mfgData.VendorName != "" {
		fields = append(fields, zap.String("vendor", mfgData.VendorName))
	}
	if mfgData.DeviceCode != "" {
		fields = append(fields, zap.String("model", mfgData.DeviceCode))
	}
	if mfgData.SerialNumber != "" {
		fields = append(fields, zap.String("serial", mfgData.SerialNumber))
	}
	if mfgData.SoftwareRevision != "" {
		fields = append(fields, zap.String("software", mfgData.SoftwareRevision))
	}

	if len(fields) > 0 {
		c.logger.Info("Vehicle manufacturer data received", fields...)
	}
}

// updateChargeState updates the charging state when the EV reports state changes
func (c *Charger) updateChargeState() {
	if c.evEntity == nil {
		return
	}

	chargeState, err := c.evCC.ChargeState(c.evEntity)
	if err != nil {
		return
	}

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

// updateSessionEnergy updates the cached session energy value
func (c *Charger) updateSessionEnergy() {
	if c.evEntity == nil {
		return
	}

	if c.evCem.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		if energy, err := c.evCem.EnergyCharged(c.evEntity); err == nil {
			c.sessionEnergy = energy / 1000.0 // Convert Wh to kWh
		}
	}
}

// updateCurrentPerPhase updates the cached current per phase values
func (c *Charger) updateCurrentPerPhase() {
	if c.evEntity == nil {
		return
	}

	if c.evCem.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		if currents, err := c.evCem.CurrentPerPhase(c.evEntity); err == nil {
			c.currentPerPhase = currents
		}
	}
}

// updatePowerPerPhase updates the cached power per phase values
func (c *Charger) updatePowerPerPhase() {
	if c.evEntity == nil {
		return
	}

	if c.evCem.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		if power, err := c.evCem.PowerPerPhase(c.evEntity); err == nil {
			c.powerPerPhase = power
		}
	}
}

// updateStateOfCharge updates the cached vehicle SoC
func (c *Charger) updateStateOfCharge() {
	if c.evEntity == nil {
		return
	}

	if c.evSoc.IsScenarioAvailableAtEntity(c.evEntity, 1) {
		if soc, err := c.evSoc.StateOfCharge(c.evEntity); err == nil {
			c.vehicleSoC = &soc
			c.logger.Info("Vehicle SoC updated", zap.Float64("soc", soc))
		}
	}
}

// isActuallyCharging checks if the EV is actually drawing power
func (c *Charger) isActuallyCharging() bool {
	if len(c.currentPerPhase) == 0 {
		return false
	}

	// Consider charging if any phase has significant current (>1A)
	for _, current := range c.currentPerPhase {
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

	// Calculate total power from cached measurements
	var chargePower float64
	if len(c.powerPerPhase) > 0 {
		// Use cached power measurements if available
		for _, power := range c.powerPerPhase {
			chargePower += power
		}
	} else if len(c.currentPerPhase) > 0 {
		// Fallback: calculate from current (I × V)
		for _, current := range c.currentPerPhase {
			chargePower += current * voltage
		}
	}

	// Get friendly vehicle name from config if available
	vehicleName := ""
	if c.vehicleNames != nil && c.vehicleID != "" {
		if friendlyName, ok := c.vehicleNames[c.vehicleID]; ok {
			vehicleName = friendlyName
		}
	}

	state := &mqtt.ChargerState{
		VehicleConnected:  c.isConnected,
		VehicleIdentity:   c.vehicleID,
		VehicleName:       vehicleName,
		Charging:          c.chargingState == ChargingStateActive && chargePower > 100,
		ChargingState:     string(c.chargingState),
		ChargePower:       chargePower,
		CurrentLimit:      c.currentLimit,
		SessionEnergy:     c.sessionEnergy, // Cached from DataUpdateEnergyCharged events
		VehicleSoC:        c.vehicleSoC, // Cached from DataUpdateStateOfCharge events
		ChargeRemainingEnergy: nil, // Not available from ISO 15118
	}

	if err := c.mqttHandler.PublishChargerState(c.config.Name, state); err != nil {
		c.logger.Warn("Failed to publish MQTT state", zap.Error(err))
	}
}
