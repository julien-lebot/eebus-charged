package mqtt

import (
	"encoding/json"
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// Publisher handles MQTT message publishing
type Publisher struct {
	client      mqtt.Client
	topicPrefix string
	logger      *zap.Logger
	enabled     bool
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

// NewPublisher creates a new MQTT publisher
func NewPublisher(broker string, port int, username, password, clientID, topicPrefix string, logger *zap.Logger) (*Publisher, error) {
	span := tracer.StartSpan("mqtt.new_publisher")
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

	logger.Info("MQTT publisher initialized", zap.String("broker", broker), zap.String("topic_prefix", topicPrefix))

	return &Publisher{
		client:      client,
		topicPrefix: topicPrefix,
		logger:      logger,
		enabled:     true,
	}, nil
}

// PublishChargerState publishes the complete charger state
func (p *Publisher) PublishChargerState(chargerName string, state *ChargerState) error {
	if !p.enabled {
		return nil
	}

	span := tracer.StartSpan("mqtt.publish_charger_state", tracer.Tag("charger", chargerName))
	defer span.Finish()

	state.Timestamp = time.Now()

	// Publish individual topics for Home Assistant auto-discovery compatibility
	baseTopic := fmt.Sprintf("%s/chargers/%s", p.topicPrefix, chargerName)

	// Binary sensors
	if err := p.publish(fmt.Sprintf("%s/connected", baseTopic), state.VehicleConnected); err != nil {
		return err
	}
	if err := p.publish(fmt.Sprintf("%s/charging", baseTopic), state.Charging); err != nil {
		return err
	}

	// Sensors
	if state.VehicleConnected {
		if state.VehicleIdentity != "" {
			if err := p.publish(fmt.Sprintf("%s/vehicle_identity", baseTopic), state.VehicleIdentity); err != nil {
				return err
			}
		}
		if state.VehicleName != "" {
			if err := p.publish(fmt.Sprintf("%s/vehicle_name", baseTopic), state.VehicleName); err != nil {
				return err
			}
		}
		if state.VehicleSoC != nil {
			if err := p.publish(fmt.Sprintf("%s/vehicle_soc", baseTopic), *state.VehicleSoC); err != nil {
				return err
			}
		}
	}

	if err := p.publish(fmt.Sprintf("%s/charge_power", baseTopic), state.ChargePower); err != nil {
		return err
	}
	if err := p.publish(fmt.Sprintf("%s/current_limit", baseTopic), state.CurrentLimit); err != nil {
		return err
	}
	if err := p.publish(fmt.Sprintf("%s/session_energy", baseTopic), state.SessionEnergy); err != nil {
		return err
	}
	if state.ChargeRemainingEnergy != nil {
		if err := p.publish(fmt.Sprintf("%s/charge_remaining_energy", baseTopic), *state.ChargeRemainingEnergy); err != nil {
			return err
		}
	}

	// Publish complete state as JSON for custom integrations
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	if err := p.publishRaw(fmt.Sprintf("%s/state", baseTopic), stateJSON); err != nil {
		return err
	}

	return nil
}

// publish publishes a value to a topic (handles different types)
func (p *Publisher) publish(topic string, value interface{}) error {
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

	token := p.client.Publish(topic, 0, true, payload) // QoS 0, retained
	if token.Wait() && token.Error() != nil {
		p.logger.Error("Failed to publish MQTT message", zap.String("topic", topic), zap.Error(token.Error()))
		return token.Error()
	}

	p.logger.Debug("Published MQTT message", zap.String("topic", topic), zap.String("payload", payload))
	return nil
}

// publishRaw publishes raw bytes to a topic
func (p *Publisher) publishRaw(topic string, payload []byte) error {
	token := p.client.Publish(topic, 0, true, payload) // QoS 0, retained
	if token.Wait() && token.Error() != nil {
		p.logger.Error("Failed to publish MQTT message", zap.String("topic", topic), zap.Error(token.Error()))
		return token.Error()
	}

	p.logger.Debug("Published MQTT message", zap.String("topic", topic), zap.Int("size", len(payload)))
	return nil
}

// Close closes the MQTT connection
func (p *Publisher) Close() {
	if p.client != nil && p.client.IsConnected() {
		p.client.Disconnect(250)
		p.logger.Info("MQTT publisher closed")
	}
}

