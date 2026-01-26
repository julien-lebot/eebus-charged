package eebus

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	eebusapi "github.com/enbility/eebus-go/api"
	"github.com/enbility/eebus-go/service"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/cem/evcc"
	"github.com/enbility/eebus-go/usecases/cem/evcem"
	"github.com/enbility/eebus-go/usecases/cem/evsoc"
	"github.com/enbility/eebus-go/usecases/cem/opev"
	"github.com/enbility/eebus-go/usecases/cem/oscev"
	shipapi "github.com/enbility/ship-go/api"
	"github.com/enbility/ship-go/cert"
	"github.com/enbility/ship-go/mdns"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"
	"github.com/julienar/eebus-charged/internal/config"
	"github.com/julienar/eebus-charged/internal/mqtt"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// Service wraps the EEBUS service and manages charger connections
type Service struct {
	config  *config.Config
	logger  *zap.Logger
	service eebusapi.ServiceInterface
	mqttHandler *mqtt.MqttHandler

	// Use cases
	evCC  ucapi.CemEVCCInterface  // EV Charging Control
	evCem ucapi.CemEVCEMInterface // EV Commissioning and Energy Management
	evSoc ucapi.CemEVSOCInterface // EV State of Charge (ISO 15118-20 or ISO 15118-2 with VAS)
	opEV  ucapi.CemOPEVInterface  // Overload Protection
	oscEV ucapi.CemOSCEVInterface // Optimization of Self Consumption

	chargers         map[string]*Charger
	discoveredDevices map[string]shipapi.RemoteService // SKI -> discovered device info
	mu               sync.RWMutex
}

// NewService creates a new EEBUS service instance
func NewService(cfg *config.Config, logger *zap.Logger, mqttHandler *mqtt.MqttHandler) (*Service, error) {
	span := tracer.StartSpan("eebus.new_service")
	defer span.Finish()

	s := &Service{
		config:            cfg,
		logger:            logger,
		mqttHandler:       mqttHandler,
		chargers:          make(map[string]*Charger),
		discoveredDevices: make(map[string]shipapi.RemoteService),
	}

	// Ensure certificates exist
	if err := s.ensureCertificates(); err != nil {
		return nil, fmt.Errorf("failed to ensure certificates: %w", err)
	}

	// Load certificate
	certificate, err := tls.LoadX509KeyPair(cfg.Certificates.CertFile, cfg.Certificates.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificates: %w", err)
	}

	// Parse the certificate to populate the Leaf field
	if len(certificate.Certificate) == 0 {
		return nil, fmt.Errorf("no certificate found in certificate chain")
	}
	
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Create EEBUS service configuration
	ski, err := cert.SkiFromCertificate(certificate.Leaf)
	if err != nil {
		return nil, fmt.Errorf("failed to get SKI from certificate: %w", err)
	}

	configuration, err := eebusapi.NewConfiguration(
		cfg.Service.Brand,  // vendor code
		cfg.Service.Brand,  // device brand
		cfg.Service.Model,  // device model
		cfg.Service.Serial, // serial number
		model.DeviceTypeTypeEnergyManagementSystem,      // device type
		[]model.EntityTypeType{model.EntityTypeTypeCEM}, // entity types
		cfg.Network.Port,  // port
		certificate,       // TLS certificate
		time.Second*4,     // heartbeat timeout
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create EEBUS configuration: %w", err)
	}

	// Configure mDNS the same way as EVCC
	configuration.SetMdnsProviderSelection(mdns.MdnsProviderSelectionAll)
	configuration.SetAlternateIdentifier(cfg.Service.Serial)
	configuration.SetAlternateMdnsServiceName("EEBUS_HEMS_" + cfg.Service.Serial)

	// Create the EEBUS service
	s.service = service.NewService(configuration, s)
	s.service.SetLogging(s)

	if err := s.service.Setup(); err != nil {
		return nil, fmt.Errorf("failed to setup EEBUS service: %w", err)
	}

	// Get the local CEM entity
	localEntity := s.service.LocalDevice().EntityForType(model.EntityTypeTypeCEM)

	// Initialize use cases
	s.evCC = evcc.NewEVCC(s.service, localEntity, s.useCaseCallback)
	s.evCem = evcem.NewEVCEM(s.service, localEntity, s.useCaseCallback)
	s.evSoc = evsoc.NewEVSOC(localEntity, s.useCaseCallback)
	s.opEV = opev.NewOPEV(localEntity, s.useCaseCallback)
	s.oscEV = oscev.NewOSCEV(localEntity, s.useCaseCallback)

	// Register use cases with the service
	s.service.AddUseCase(s.evCC)
	s.service.AddUseCase(s.evCem)
	s.service.AddUseCase(s.evSoc)
	s.service.AddUseCase(s.opEV)
	s.service.AddUseCase(s.oscEV)

	logger.Info("EEBUS service created", zap.String("ski", ski))

	return s, nil
}

// Start starts the EEBUS service
func (s *Service) Start() error {
	s.logger.Info("Starting EEBUS service",
		zap.String("brand", s.config.Service.Brand),
		zap.String("model", s.config.Service.Model),
		zap.Int("port", s.config.Network.Port),
	)


	// If chargers are configured in config.yaml, wait for them to be discovered
	// then connect (they're already paired, just reconnecting)
	if len(s.config.Chargers) > 0 {
		s.logger.Info("Will connect to configured chargers when discovered via mDNS", zap.Int("count", len(s.config.Chargers)))
	} else {
		s.logger.Info("No chargers configured - waiting for new devices to pair")
	}

	// Start the service
	s.service.Start()

	// Publish initial charger list (for configured chargers that may already be connected)
	s.publishChargerList()

	s.logger.Info("EEBUS service started successfully")
	return nil
}

// Stop stops the EEBUS service
func (s *Service) Stop() {
	s.logger.Info("Stopping EEBUS service")
	s.service.Shutdown()
}

// GetCharger returns a charger by name
func (s *Service) GetCharger(name string) (*Charger, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	charger, ok := s.chargers[name]
	if !ok {
		return nil, fmt.Errorf("charger %s not found", name)
	}

	return charger, nil
}

// ListChargers returns all configured chargers
func (s *Service) ListChargers() []*Charger {
	s.mu.RLock()
	defer s.mu.RUnlock()

	chargers := make([]*Charger, 0, len(s.chargers))
	for _, c := range s.chargers {
		chargers = append(chargers, c)
	}

	return chargers
}

// CommandHandler implementation for MQTT commands

// HandleStart starts charging on the specified charger
func (s *Service) HandleStart(chargerName string) error {
	charger, err := s.GetCharger(chargerName)
	if err != nil {
		return err
	}
	return charger.StartCharging()
}

// HandleStop stops charging on the specified charger
func (s *Service) HandleStop(chargerName string) error {
	charger, err := s.GetCharger(chargerName)
	if err != nil {
		return err
	}
	return charger.StopCharging()
}

// HandleSetCurrent sets the current limit on the specified charger
func (s *Service) HandleSetCurrent(chargerName string, current float64) error {
	charger, err := s.GetCharger(chargerName)
	if err != nil {
		return err
	}
	return charger.SetCurrentLimit(current)
}

// ensureCertificates generates certificates if they don't exist
func (s *Service) ensureCertificates() error {
	certFile := s.config.Certificates.CertFile
	keyFile := s.config.Certificates.KeyFile

	// Check if certificates already exist
	if _, err := os.Stat(certFile); err == nil {
		if _, err := os.Stat(keyFile); err == nil {
			s.logger.Info("Using existing certificates")
			return nil
		}
	}

	s.logger.Info("Generating new certificates")

	// Use the ship-go cert.CreateCertificate function which properly adds SKI extension
	certificate, err := cert.CreateCertificate(
		s.config.Service.Brand,
		s.config.Service.Brand,
		"DE", // Country
		s.config.Service.Serial,
	)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	// Write certificate to file
	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("failed to create certificate file: %w", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}); err != nil {
		return fmt.Errorf("failed to write certificate: %w", err)
	}

	// Write private key to file
	keyOut, err := os.Create(keyFile)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(certificate.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return fmt.Errorf("failed to write private key: %w", err)
	}

	s.logger.Info("Certificates generated successfully",
		zap.String("cert_file", certFile),
		zap.String("key_file", keyFile),
	)

	return nil
}

// EEBUS service callbacks - implements eebusapi.ServiceReaderInterface

// RemoteSKIConnected is called when a remote device connects
func (s *Service) RemoteSKIConnected(service eebusapi.ServiceInterface, ski string) {
	s.logger.Info("✓ Remote device CONNECTED successfully", zap.String("ski", ski))
	// Device will be created when first use case event fires
	// Don't block here - the SPINE layer needs this callback to return
}

// RemoteSKIDisconnected is called when a remote device disconnects
func (s *Service) RemoteSKIDisconnected(service eebusapi.ServiceInterface, ski string) {
	s.logger.Info("Remote SKI disconnected", zap.String("ski", ski))
	s.removeCharger(ski)
}

// VisibleRemoteServicesUpdated is called when visible services change  
func (s *Service) VisibleRemoteServicesUpdated(service eebusapi.ServiceInterface, entries []shipapi.RemoteService) {
	s.logger.Debug("Visible services updated", zap.Int("count", len(entries)))
	
	s.mu.Lock()
	defer s.mu.Unlock()
	
	for _, entry := range entries {
		ski := normalizeSkI(entry.Ski)
		
		// Store discovered device info (only log once)
		if _, exists := s.discoveredDevices[ski]; !exists {
			s.discoveredDevices[ski] = entry
			s.logger.Info("Discovered EEBUS device via mDNS",
				zap.String("ski", ski),
				zap.String("name", entry.Name),
				zap.String("brand", entry.Brand),
				zap.String("model", entry.Model),
			)
			
			// Check if this is a configured (already-paired) charger
			for _, chargerCfg := range s.config.Chargers {
				if normalizeSkI(chargerCfg.SKI) == ski {
					s.logger.Info("Reconnecting to configured charger", zap.String("name", chargerCfg.Name))
					s.service.RegisterRemoteSKI(ski)
					
					// Set IP to bypass DNS
					if chargerCfg.IP != "" {
						if rs := s.service.RemoteServiceForSKI(ski); rs != nil {
							rs.SetIPv4(chargerCfg.IP)
						}
					}
					break
				}
			}
		}
	}
}

// ServiceShipIDUpdate is called when a SHIP ID is updated
func (s *Service) ServiceShipIDUpdate(ski string, shipID string) {
	s.logger.Info("✓ SHIP ID RECEIVED - Device is now trusted",
		zap.String("ski", ski),
		zap.String("ship_id", shipID),
	)
	
	// Create charger now that connection is established
	// Get the device from SPINE layer
	localDevice := s.service.LocalDevice()
	if localDevice == nil {
		s.logger.Error("Failed to get local device")
		return
	}
	
	device := localDevice.RemoteDeviceForSki(ski)
	if device == nil {
		s.logger.Warn("Device not yet in SPINE layer, will retry")
		// Retry after a short delay
		go func() {
			time.Sleep(200 * time.Millisecond)
			device = localDevice.RemoteDeviceForSki(ski)
			if device == nil {
				s.logger.Error("Device still not available after delay")
				return
			}
			s.createChargerForDevice(ski, device)
		}()
		return
	}
	
	s.createChargerForDevice(ski, device)
}

// createChargerForDevice creates a charger instance for a connected device
func (s *Service) createChargerForDevice(ski string, device spineapi.DeviceRemoteInterface) {
	// Check if charger already exists
	s.mu.RLock()
	_, exists := s.findChargerBySKI(ski)
	s.mu.RUnlock()
	
	if exists {
		s.logger.Debug("Charger already exists", zap.String("ski", ski))
		return
	}
	
	// Get charger config
	var cfg config.ChargerConfig
	found := false
	normalizedSki := normalizeSkI(ski)
	
	for _, chargerCfg := range s.config.Chargers {
		if normalizeSkI(chargerCfg.SKI) == normalizedSki {
			cfg = chargerCfg
			found = true
			break
		}
	}
	
	if !found {
		suffix := normalizedSki
		if len(suffix) > 8 {
			suffix = suffix[len(suffix)-8:]
		}
		cfg = config.ChargerConfig{
			Name: "charger_" + suffix,
			SKI:  ski,
		}
	}
	
	s.addCharger(cfg, device)
}

// ServicePairingDetailUpdate is called when pairing details change
func (s *Service) ServicePairingDetailUpdate(ski string, detail *shipapi.ConnectionStateDetail) {
	state := detail.State()
	s.logger.Info("Pairing detail update",
		zap.String("ski", ski),
		zap.String("state", fmt.Sprintf("%v", state)),
		zap.Int("state_code", int(state)),
	)
	
	// Auto-accept pairing requests
	if state == shipapi.ConnectionStateReceivedPairingRequest {
		s.logger.Info("Accepting pairing request", zap.String("ski", ski))
		s.service.RegisterRemoteSKI(ski)
		return
	}
	
	// If the remote denied trust, we might need to re-initiate
	if state == shipapi.ConnectionStateRemoteDeniedTrust {
		s.logger.Warn("Remote device denied trust - pairing failed",
			zap.String("ski", ski))
	}
}

// AllowWaitingForTrust determines if we should wait for trust from a device
func (s *Service) AllowWaitingForTrust(ski string) bool {
	// Auto-accept all pairing requests in home environment
	s.logger.Info("Pairing request received - auto-accepting", zap.String("ski", ski))
	
	// Register the remote device to establish connection
	s.service.RegisterRemoteSKI(ski)
	
	return true
}

// useCaseCallback is called when use case events occur
func (s *Service) useCaseCallback(ski string, device spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	s.logger.Debug("Use case event",
		zap.String("ski", ski),
		zap.String("event", string(event)),
	)

	// Check if charger exists
	s.mu.RLock()
	charger, exists := s.findChargerBySKI(ski)
	s.mu.RUnlock()

	if !exists {
		// Charger not yet created - create it now with the device
		s.logger.Info("Creating charger from use case event", zap.String("ski", ski))
		
		// Check if this SKI is in config
		var cfg config.ChargerConfig
		found := false
		normalizedSki := normalizeSkI(ski)
		
		for _, chargerCfg := range s.config.Chargers {
			if normalizeSkI(chargerCfg.SKI) == normalizedSki {
				cfg = chargerCfg
				found = true
				break
			}
		}
		
		// If not in config, auto-generate name
		if !found {
			suffix := normalizedSki
			if len(suffix) > 8 {
				suffix = suffix[len(suffix)-8:]
			}
			cfg = config.ChargerConfig{
				Name: "charger_" + suffix,
				SKI:  ski,
			}
		}
		
		s.addCharger(cfg, device)
		
		// Reload charger after adding
		s.mu.RLock()
		charger, exists = s.findChargerBySKI(ski)
		s.mu.RUnlock()
		
		if !exists {
			s.logger.Error("Failed to create charger", zap.String("ski", ski))
			return
		}
	}

	// Notify charger of event
	charger.handleUseCaseEvent(device, entity, event)
}

// findChargerBySKI finds a charger by SKI (caller must hold lock)
func (s *Service) findChargerBySKI(ski string) (*Charger, bool) {
	for _, charger := range s.chargers {
		if charger.device.Ski() == ski {
			return charger, true
		}
	}
	return nil, false
}

// addCharger adds a charger to the service
func (s *Service) addCharger(cfg config.ChargerConfig, device spineapi.DeviceRemoteInterface) {
	s.mu.Lock()
	charger := NewCharger(cfg, device, s.logger, &s.config.Charging, s.evCC, s.evCem, s.evSoc, s.opEV, s.oscEV, s.mqttHandler, s.config.Vehicles)
	s.chargers[cfg.Name] = charger
	s.mu.Unlock()

	s.logger.Info("Charger added",
		zap.String("name", cfg.Name),
		zap.String("ski", device.Ski()),
	)

	// Publish updated charger list
	s.publishChargerList()
}

// removeCharger removes a charger from the service
func (s *Service) removeCharger(ski string) {
	s.mu.Lock()
	var removedName string
	for name, charger := range s.chargers {
		if charger.device.Ski() == ski {
			delete(s.chargers, name)
			removedName = name
			break
		}
	}
	s.mu.Unlock()

	if removedName != "" {
		s.logger.Info("Charger removed",
			zap.String("name", removedName),
			zap.String("ski", ski),
		)
		// Publish updated charger list
		s.publishChargerList()
	}
}

// publishChargerList publishes the list of available chargers to MQTT
func (s *Service) publishChargerList() {
	if s.mqttHandler == nil {
		return
	}

	s.mu.RLock()
	chargerInfos := make([]mqtt.ChargerInfo, 0, len(s.chargers))
	for name, charger := range s.chargers {
		chargerInfos = append(chargerInfos, mqtt.ChargerInfo{
			Name:      name,
			SKI:       charger.SKI(),
			Connected: charger.IsConnected(),
		})
	}
	s.mu.RUnlock()

	if err := s.mqttHandler.PublishChargerList(chargerInfos); err != nil {
		s.logger.Warn("Failed to publish charger list", zap.Error(err))
	}
}

// Logging interface implementation for EEBUS

func (s *Service) Trace(args ...interface{}) {
	// Trace logging is too verbose, skip it
}

func (s *Service) Tracef(format string, args ...interface{}) {
	// Trace logging is too verbose, skip it
}

func (s *Service) Debug(args ...interface{}) {
	s.logger.Debug(fmt.Sprint(args...))
}

func (s *Service) Debugf(format string, args ...interface{}) {
	s.logger.Debug(fmt.Sprintf(format, args...))
}

func (s *Service) Info(args ...interface{}) {
	s.logger.Info(fmt.Sprint(args...))
}

func (s *Service) Infof(format string, args ...interface{}) {
	s.logger.Info(fmt.Sprintf(format, args...))
}

func (s *Service) Error(args ...interface{}) {
	s.logger.Error(fmt.Sprint(args...))
}

func (s *Service) Errorf(format string, args ...interface{}) {
	s.logger.Error(fmt.Sprintf(format, args...))
}

// normalizeSkI removes spaces and dashes from SKI
func normalizeSkI(ski string) string {
	var sb strings.Builder
	sb.Grow(len(ski))
	for _, c := range ski {
		if c != ' ' && c != '-' {
			sb.WriteRune(c)
		}
	}
	return sb.String()
}
