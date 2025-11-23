package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/julienar/eebus-charged/internal/api"
	"github.com/julienar/eebus-charged/internal/config"
	"github.com/julienar/eebus-charged/internal/eebus"
	"github.com/julienar/eebus-charged/internal/mqtt"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the EEBUS HEMS service",
	Long: `Start the EEBUS HEMS service and begin monitoring/controlling chargers.
	
The service will:
- Connect to configured EEBUS chargers
- Monitor vehicle connections
- Handle vehicle identification
- Accept control commands via the CLI`,
	RunE: runService,
}

func init() {
	rootCmd.AddCommand(runCmd)
}

func runService(cmd *cobra.Command, args []string) error {
	// Load configuration first
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Create logger from config
	logger, err := CreateLoggerFromConfig(cfg.Logging)
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer logger.Sync()

	// Initialize Datadog tracing if enabled
	if cfg.Datadog.Enabled {
		tracer.Start(
			tracer.WithService(cfg.Datadog.ServiceName),
			tracer.WithEnv(cfg.Datadog.Environment),
			tracer.WithAgentAddr(fmt.Sprintf("%s:%d", cfg.Datadog.AgentHost, cfg.Datadog.AgentPort)),
		)
		defer tracer.Stop()
		logger.Info("Datadog tracing initialized",
			zap.String("service", cfg.Datadog.ServiceName),
			zap.String("environment", cfg.Datadog.Environment),
		)
	}

	logger.Info("Starting EEBUS HEMS")
	logger.Info("Configuration loaded",
		zap.Int("chargers", len(cfg.Chargers)),
		zap.Float64("min_current", cfg.Charging.MinCurrent),
		zap.Float64("max_current", cfg.Charging.MaxCurrent),
		zap.Bool("mqtt_enabled", cfg.MQTT.Enabled),
		zap.Bool("datadog_enabled", cfg.Datadog.Enabled),
	)

	// Initialize MQTT publisher if enabled
	var mqttPub *mqtt.Publisher
	if cfg.MQTT.Enabled {
		mqttPub, err = mqtt.NewPublisher(
			cfg.MQTT.Broker,
			cfg.MQTT.Port,
			cfg.MQTT.Username,
			cfg.MQTT.Password,
			cfg.MQTT.ClientID,
			cfg.MQTT.TopicPrefix,
			logger,
		)
		if err != nil {
			return fmt.Errorf("failed to initialize MQTT publisher: %w", err)
		}
		defer mqttPub.Close()
		logger.Info("MQTT publisher initialized",
			zap.String("broker", cfg.MQTT.Broker),
			zap.String("topic_prefix", cfg.MQTT.TopicPrefix),
		)
	}

	// Create EEBUS service
	service, err := eebus.NewService(cfg, logger, mqttPub)
	if err != nil {
		return fmt.Errorf("failed to create EEBUS service: %w", err)
	}

	// Start service
	if err := service.Start(); err != nil {
		return fmt.Errorf("failed to start EEBUS service: %w", err)
	}

	// Start API server for control commands
	apiAddr := fmt.Sprintf("localhost:%d", cfg.Network.APIPort)
	apiServer := api.NewServer(service, logger, apiAddr)
	go func() {
		if err := apiServer.Start(); err != nil {
			logger.Error("API server failed", zap.Error(err))
		}
	}()

	logger.Info("EEBUS HEMS is running. Press Ctrl+C to stop.")
	logger.Info("API server listening", zap.String("url", fmt.Sprintf("http://%s", apiAddr)))
	logger.Info("Waiting for chargers to connect via mDNS...")
	logger.Info("Put your chargers in pairing mode to connect")

	// Wait for interrupt signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		logger.Info("Received shutdown signal")
	case <-ctx.Done():
		logger.Info("Context cancelled")
	}

	// Shutdown service
	logger.Info("Shutting down EEBUS service")
	service.Stop()

	logger.Info("EEBUS HEMS stopped")
	return nil
}

