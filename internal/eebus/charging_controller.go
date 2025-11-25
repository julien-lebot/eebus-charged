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

// ChargingController handles charging control for a specific communication standard
type ChargingController interface {
	// WriteCurrentLimit writes a current limit to the EV
	// current=0 should pause charging if possible
	WriteCurrentLimit(evEntity spineapi.EntityRemoteInterface, current float64) error
	
	// Name returns the controller name for logging
	Name() string
}

// createController creates the appropriate controller for the communication standard
func createController(
	commStd string,
	chargingCfg *config.ChargingConfig,
	evCC ucapi.CemEVCCInterface,
	opEV ucapi.CemOPEVInterface,
	oscEV ucapi.CemOSCEVInterface,
	logger *zap.Logger,
) ChargingController {
	
	switch commStd {
	case "iec61851":
		return newIec61851Controller(chargingCfg, opEV, logger)
		
	case "iso15118-2ed2":
		// Check if VAS is supported
		return newIso15118ed2Controller(chargingCfg, evCC, opEV, oscEV, logger)
		
	default:
		logger.Debug("Unknown communication standard, using fallback", zap.String("standard", commStd))
		return newIec61851Controller(chargingCfg, opEV, logger)
	}
}

