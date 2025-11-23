package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config represents the application configuration
type Config struct {
	Service      ServiceConfig      `mapstructure:"service"`
	Network      NetworkConfig      `mapstructure:"network"`
	Certificates CertificatesConfig `mapstructure:"certificates"`
	Chargers     []ChargerConfig    `mapstructure:"chargers"` // Optional
	Charging     ChargingConfig     `mapstructure:"charging"`
	Logging      LoggingConfig      `mapstructure:"logging"`
	Datadog      DatadogConfig      `mapstructure:"datadog"`
	MQTT         MQTTConfig         `mapstructure:"mqtt"`
}

// ServiceConfig contains service identification
type ServiceConfig struct {
	Brand      string `mapstructure:"brand"`
	Model      string `mapstructure:"model"`
	Serial     string `mapstructure:"serial"`
	DeviceName string `mapstructure:"device_name"`
}

// NetworkConfig contains network settings
type NetworkConfig struct {
	Port    int    `mapstructure:"port"`     // EEBUS communication port
	APIPort int    `mapstructure:"api_port"` // HTTP API port
	Interface string `mapstructure:"interface"`
}

// CertificatesConfig contains certificate paths
type CertificatesConfig struct {
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

// ChargerConfig contains charger information (optional manual configuration)
type ChargerConfig struct {
	Name string `mapstructure:"name"`
	SKI  string `mapstructure:"ski"`
	IP   string `mapstructure:"ip"` // Optional: for direct IP connection when mDNS fails
}

// ChargingConfig contains charging parameters
type ChargingConfig struct {
	MinCurrent     float64 `mapstructure:"min_current"`
	MaxCurrent     float64 `mapstructure:"max_current"`
	DefaultCurrent float64 `mapstructure:"default_current"`
	Phases         int     `mapstructure:"phases"`
}

// LoggingConfig contains logging settings
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	File   string `mapstructure:"file"` // Optional: log file path
}

// DatadogConfig contains Datadog APM settings
type DatadogConfig struct {
	Enabled     bool   `mapstructure:"enabled"`
	AgentHost   string `mapstructure:"agent_host"`
	AgentPort   int    `mapstructure:"agent_port"`
	ServiceName string `mapstructure:"service_name"`
	Environment string `mapstructure:"environment"`
}

// MQTTConfig contains MQTT broker settings
type MQTTConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	Broker   string `mapstructure:"broker"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	ClientID string `mapstructure:"client_id"`
	TopicPrefix string `mapstructure:"topic_prefix"` // e.g., "hems" -> "hems/chargers/garage/..."
}

// Load loads configuration from file
func Load(configPath string) (*Config, error) {
	v := viper.New()

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.eebus-charged")
		v.AddConfigPath("/etc/eebus-charged")
	}

	// Set defaults
	v.SetDefault("service.brand", "Custom HEMS")
	v.SetDefault("service.model", "EEBUS-CHARGED v1.0")
	v.SetDefault("service.serial", "HEMS-001")
	v.SetDefault("service.device_name", "Home Energy Manager")
	v.SetDefault("network.port", 4712)
	v.SetDefault("network.api_port", 8080)
	v.SetDefault("certificates.cert_file", "./certs/cert.pem")
	v.SetDefault("certificates.key_file", "./certs/key.pem")
	v.SetDefault("charging.min_current", 6.0)
	v.SetDefault("charging.max_current", 32.0)
	v.SetDefault("charging.default_current", 16.0)
	v.SetDefault("charging.phases", 3)
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "console")
	v.SetDefault("datadog.enabled", false)
	v.SetDefault("datadog.agent_host", "localhost")
	v.SetDefault("datadog.agent_port", 8126)
	v.SetDefault("datadog.service_name", "eebus-charged")
	v.SetDefault("datadog.environment", "production")
	v.SetDefault("mqtt.enabled", false)
	v.SetDefault("mqtt.broker", "localhost")
	v.SetDefault("mqtt.port", 1883)
	v.SetDefault("mqtt.client_id", "eebus-charged")
	v.SetDefault("mqtt.topic_prefix", "hems")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			fmt.Fprintf(os.Stderr, "Warning: Config file not found, using defaults\n")
		} else {
			return nil, fmt.Errorf("error reading config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	// Ensure certificate directory exists
	certDir := filepath.Dir(cfg.Certificates.CertFile)
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return nil, fmt.Errorf("error creating certificate directory: %w", err)
	}

	return &cfg, nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Service.Serial == "" {
		return fmt.Errorf("service.serial is required")
	}

	if c.Network.Port < 1 || c.Network.Port > 65535 {
		return fmt.Errorf("network.port must be between 1 and 65535")
	}

	if c.Network.APIPort < 1 || c.Network.APIPort > 65535 {
		return fmt.Errorf("network.api_port must be between 1 and 65535")
	}

	if c.Charging.MinCurrent < 0 {
		return fmt.Errorf("charging.min_current must be positive")
	}

	if c.Charging.MaxCurrent < c.Charging.MinCurrent {
		return fmt.Errorf("charging.max_current must be greater than min_current")
	}

	if c.Charging.Phases != 1 && c.Charging.Phases != 3 {
		return fmt.Errorf("charging.phases must be 1 or 3")
	}

	return nil
}

