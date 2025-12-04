package eebus

import (
	"errors"

	ucapi "github.com/enbility/eebus-go/usecases/api"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/julienar/eebus-charged/internal/config"
	"go.uber.org/zap"
)

var (
	// ErrOverloadProtectionUnavailable is returned when OPEV is not available
	ErrOverloadProtectionUnavailable = errors.New("overload protection not available")
)

// LimitsProvider provides cached current limits to avoid deadlock from calling CurrentLimits()
type LimitsProvider interface {
	// GetOPEVLimits returns cached OPEV min/max limits per phase
	GetOPEVLimits() (minLimits, maxLimits []float64)
	// GetOSCEVLimits returns cached OSCEV min/max limits per phase
	GetOSCEVLimits() (minLimits, maxLimits []float64)
}

// ControllerCapabilities represents the capabilities of a charging controller
type ControllerCapabilities struct {
	CommunicationStandard string `json:"communication_standard"`
	ControllerType        string `json:"controller_type"`
	VASSupported          *bool  `json:"vas_supported,omitempty"` // nil if not applicable
	OSCEVAvailable        *bool  `json:"oscev_available,omitempty"` // nil if not applicable
	OPEVAvailable         *bool  `json:"opev_available,omitempty"`  // nil if not applicable
}

// ChargingController handles charging control for a specific communication standard
type ChargingController interface {
	// WriteCurrentLimit writes a current limit to the EV
	// current=0 should pause charging if possible
	WriteCurrentLimit(evEntity spineapi.EntityRemoteInterface, current float64) error
	
	// Name returns the controller name for logging
	Name() string
	
	// GetCapabilities returns the capabilities of this controller
	GetCapabilities(evEntity spineapi.EntityRemoteInterface) ControllerCapabilities
}

// createController creates the appropriate controller for the communication standard
func createController(
	commStd string,
	chargingCfg *config.ChargingConfig,
	evCC ucapi.CemEVCCInterface,
	opEV ucapi.CemOPEVInterface,
	oscEV ucapi.CemOSCEVInterface,
	limitsProvider LimitsProvider,
	logger *zap.Logger,
) ChargingController {
	
	switch commStd {
	case "iec61851":
		return newIec61851Controller(chargingCfg, opEV, limitsProvider, logger)
		
	case "iso15118-2ed2":
		// Check if VAS is supported
		return newIso15118ed2Controller(chargingCfg, evCC, opEV, oscEV, limitsProvider, logger)
		
	default:
		logger.Debug("Unknown communication standard, using fallback", zap.String("standard", commStd))
		return newIec61851Controller(chargingCfg, opEV, limitsProvider, logger)
	}
}

