package mqtt

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// CommandHandler handles MQTT commands
type CommandHandler interface {
	HandleStart(chargerName string) error
	HandleStop(chargerName string) error
	HandleSetCurrent(chargerName string, current float64) error
}

// CommandRequest represents an MQTT command request
type CommandRequest struct {
	ResponseTopic string  `json:"response_topic,omitempty"` // Optional topic to publish response to
	Current       float64 `json:"current,omitempty"`         // For set_current command
}

// CommandResponse represents an MQTT command response
type CommandResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// MqttHandler handles MQTT message publishing and command subscriptions
type MqttHandler struct {
	client      mqtt.Client
	topicPrefix string
	logger      *zap.Logger
	enabled     bool
	handler     CommandHandler // Optional command handler
}

// ChargerState represents the state to publish for a charger
type ChargerState struct {
	// Vehicle connection
	VehicleConnected bool   `json:"vehicle_connected"`
	VehicleIdentity  string `json:"vehicle_identity,omitempty"`
	VehicleName      string `json:"vehicle_name,omitempty"`
	VehicleSoC       *float64 `json:"vehicle_soc,omitempty"` // Pointer because may not be available

	// Charging status
	Charging      bool    `json:"charging"`
	ChargingState string  `json:"charging_state"` // "active", "stopped", "unknown"
	ChargePower   float64 `json:"charge_power_w"` // Current charging power in watts
	CurrentLimit  float64 `json:"current_limit_a"` // Current limit in amps

	// Session information
	SessionEnergy         float64 `json:"session_energy_wh"`          // Total energy this session
	ChargeRemainingEnergy *float64 `json:"charge_remaining_energy_wh,omitempty"` // Estimated remaining energy

	// Timestamp
	Timestamp time.Time `json:"timestamp"`
}

// NewMqttHandler creates a new MQTT handler for publishing and subscribing
func NewMqttHandler(broker string, port int, username, password, clientID, topicPrefix string, logger *zap.Logger) (*MqttHandler, error) {
	span := tracer.StartSpan("mqtt.new_handler")
	defer span.Finish()

	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%d", broker, port))
	opts.SetClientID(clientID)
	if username != "" {
		opts.SetUsername(username)
	}
	if password != "" {
		opts.SetPassword(password)
	}
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetMaxReconnectInterval(1 * time.Minute)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		logger.Info("MQTT connected to broker", zap.String("broker", broker))
	})
	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		logger.Warn("MQTT connection lost", zap.Error(err))
	})

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("failed to connect to MQTT broker: %w", token.Error())
	}

	logger.Info("MQTT handler initialized", zap.String("broker", broker), zap.String("topic_prefix", topicPrefix))

	return &MqttHandler{
		client:      client,
		topicPrefix: topicPrefix,
		logger:      logger,
		enabled:     true,
	}, nil
}

// SubscribeToCommands subscribes to command topics and handles incoming commands
func (h *MqttHandler) SubscribeToCommands(handler CommandHandler) error {
	if !h.enabled {
		return nil
	}

	h.handler = handler

	// Subscribe to command topic: {prefix}/chargers/+/command
	// Action is in the payload: "start", "stop", or JSON for set_current
	commandTopic := fmt.Sprintf("%s/chargers/+/command", h.topicPrefix)
	token := h.client.Subscribe(commandTopic, 1, h.handleCommandMessage)
	if token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", commandTopic, token.Error())
	}

	h.logger.Info("Subscribed to MQTT command topics", zap.String("topic", commandTopic))
	return nil
}

// handleCommandMessage processes incoming MQTT command messages
func (h *MqttHandler) handleCommandMessage(client mqtt.Client, msg mqtt.Message) {
	if !h.enabled || h.handler == nil {
		return
	}

	span := tracer.StartSpan("mqtt.handle_command", tracer.Tag("topic", msg.Topic()))
	defer span.Finish()

	topic := msg.Topic()
	payload := msg.Payload()

	h.logger.Debug("Received MQTT command", zap.String("topic", topic), zap.String("payload", string(payload)))

	// Parse topic: {prefix}/chargers/{chargerName}/command
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[len(parts)-1] != "command" {
		h.logger.Warn("Invalid command topic format", zap.String("topic", topic))
		return
	}

	chargerName := parts[len(parts)-2]
	
	// Parse action from payload
	payloadStr := strings.TrimSpace(string(payload))
	if payloadStr == "" {
		h.logger.Warn("Empty payload for command topic", zap.String("topic", topic))
		return
	}
	
	var action string
	var cmdReq CommandRequest
	
	// Try to parse as JSON first (for set_current with current value)
	if err := json.Unmarshal(payload, &cmdReq); err == nil && cmdReq.Current > 0 {
		// JSON with current value = set_current command
		action = "set_current"
	} else {
		// Simple string action (start/stop)
		action = payloadStr
		// Try to parse as JSON for response_topic even if it's a string action
		json.Unmarshal(payload, &cmdReq)
	}

	span.SetTag("charger", chargerName)
	span.SetTag("action", action)

	// Execute command
	var resp CommandResponse
	switch action {
	case "start":
		err := h.handler.HandleStart(chargerName)
		if err != nil {
			resp = CommandResponse{
				Success: false,
				Message: "Failed to start charging",
				Error:   err.Error(),
			}
		} else {
			resp = CommandResponse{
				Success: true,
				Message: "Charging started",
			}
		}

	case "stop":
		err := h.handler.HandleStop(chargerName)
		if err != nil {
			resp = CommandResponse{
				Success: false,
				Message: "Failed to stop charging",
				Error:   err.Error(),
			}
		} else {
			resp = CommandResponse{
				Success: true,
				Message: "Charging stopped",
			}
		}

	case "set_current", "current":
		if cmdReq.Current <= 0 {
			resp = CommandResponse{
				Success: false,
				Message: "Invalid current value",
				Error:   "current must be greater than 0",
			}
		} else {
			err := h.handler.HandleSetCurrent(chargerName, cmdReq.Current)
			if err != nil {
				resp = CommandResponse{
					Success: false,
					Message: "Failed to set current limit",
					Error:   err.Error(),
				}
			} else {
				resp = CommandResponse{
					Success: true,
					Message: fmt.Sprintf("Current limit set to %.1fA", cmdReq.Current),
				}
			}
		}

	default:
		h.logger.Warn("Unknown command action", zap.String("action", action))
		resp = CommandResponse{
			Success: false,
			Message: "Unknown command",
			Error:   fmt.Sprintf("unknown action: %s", action),
		}
	}

	// Publish response if response_topic is provided
	if cmdReq.ResponseTopic != "" {
		respJSON, err := json.Marshal(resp)
		if err != nil {
			h.logger.Error("Failed to marshal command response", zap.Error(err))
			return
		}

		token := h.client.Publish(cmdReq.ResponseTopic, 0, false, respJSON) // QoS 0, not retained
		if token.Wait() && token.Error() != nil {
			h.logger.Error("Failed to publish command response", 
				zap.String("topic", cmdReq.ResponseTopic), 
				zap.Error(token.Error()))
		} else {
			h.logger.Debug("Published command response", 
				zap.String("topic", cmdReq.ResponseTopic),
				zap.Bool("success", resp.Success))
		}
	}
}

// PublishChargerState publishes the charger state (per-charger data)
func (h *MqttHandler) PublishChargerState(chargerName string, state *ChargerState) error {
	if !h.enabled {
		return nil
	}

	span := tracer.StartSpan("mqtt.publish_charger_state", tracer.Tag("charger", chargerName))
	defer span.Finish()

	state.Timestamp = time.Now()

	// Publish individual topics for Home Assistant auto-discovery compatibility
	baseTopic := fmt.Sprintf("%s/chargers/%s", h.topicPrefix, chargerName)

	// Binary sensors
	if err := h.publish(fmt.Sprintf("%s/connected", baseTopic), state.VehicleConnected); err != nil {
		return err
	}
	if err := h.publish(fmt.Sprintf("%s/charging", baseTopic), state.Charging); err != nil {
		return err
	}

	// Sensors
	if state.VehicleConnected {
		if state.VehicleIdentity != "" {
			if err := h.publish(fmt.Sprintf("%s/vehicle_identity", baseTopic), state.VehicleIdentity); err != nil {
				return err
			}
		}
		if state.VehicleName != "" {
			if err := h.publish(fmt.Sprintf("%s/vehicle_name", baseTopic), state.VehicleName); err != nil {
				return err
			}
		}
		if state.VehicleSoC != nil {
			if err := h.publish(fmt.Sprintf("%s/vehicle_soc", baseTopic), *state.VehicleSoC); err != nil {
				return err
			}
		}
	}

	if err := h.publish(fmt.Sprintf("%s/charge_power", baseTopic), state.ChargePower); err != nil {
		return err
	}
	if err := h.publish(fmt.Sprintf("%s/current_limit", baseTopic), state.CurrentLimit); err != nil {
		return err
	}
	if err := h.publish(fmt.Sprintf("%s/session_energy", baseTopic), state.SessionEnergy); err != nil {
		return err
	}
	if state.ChargeRemainingEnergy != nil {
		if err := h.publish(fmt.Sprintf("%s/charge_remaining_energy", baseTopic), *state.ChargeRemainingEnergy); err != nil {
			return err
		}
	}

	return nil
}

// ChargerInfo represents charger information for the list
type ChargerInfo struct {
	Name      string `json:"name"`
	SKI       string `json:"ski"`
	Connected bool   `json:"connected"`
}

// PublishChargerList publishes the list of available chargers
func (h *MqttHandler) PublishChargerList(chargers []ChargerInfo) error {
	if !h.enabled {
		return nil
	}

	span := tracer.StartSpan("mqtt.publish_charger_list")
	defer span.Finish()

	listJSON, err := json.Marshal(chargers)
	if err != nil {
		return fmt.Errorf("failed to marshal charger list: %w", err)
	}

	// Publish as retained message so clients can discover chargers on startup
	topic := fmt.Sprintf("%s/chargers", h.topicPrefix)
	token := h.client.Publish(topic, 0, true, listJSON) // QoS 0, retained
	if token.Wait() && token.Error() != nil {
		h.logger.Error("Failed to publish charger list", zap.String("topic", topic), zap.Error(token.Error()))
		return token.Error()
	}

	h.logger.Debug("Published charger list", zap.String("topic", topic), zap.Int("count", len(chargers)))
	return nil
}

// publish publishes a value to a topic (handles different types)
func (h *MqttHandler) publish(topic string, value interface{}) error {
	var payload string
	switch v := value.(type) {
	case bool:
		if v {
			payload = "true"
		} else {
			payload = "false"
		}
	case string:
		payload = v
	case float64:
		payload = fmt.Sprintf("%.2f", v)
	case int:
		payload = fmt.Sprintf("%d", v)
	default:
		payload = fmt.Sprintf("%v", v)
	}

	token := h.client.Publish(topic, 0, true, payload) // QoS 0, retained
	if token.Wait() && token.Error() != nil {
		h.logger.Error("Failed to publish MQTT message", zap.String("topic", topic), zap.Error(token.Error()))
		return token.Error()
	}

	h.logger.Debug("Published MQTT message", zap.String("topic", topic), zap.String("payload", payload))
	return nil
}

// publishRaw publishes raw bytes to a topic
func (h *MqttHandler) publishRaw(topic string, payload []byte) error {
	token := h.client.Publish(topic, 0, true, payload) // QoS 0, retained
	if token.Wait() && token.Error() != nil {
		h.logger.Error("Failed to publish MQTT message", zap.String("topic", topic), zap.Error(token.Error()))
		return token.Error()
	}

	h.logger.Debug("Published MQTT message", zap.String("topic", topic), zap.Int("size", len(payload)))
	return nil
}

// Close closes the MQTT connection
func (h *MqttHandler) Close() {
	if h.client != nil && h.client.IsConnected() {
		h.client.Disconnect(250)
		h.logger.Info("MQTT handler closed")
	}
}

