package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

// agentInfo represents the Datadog agent info response
type agentInfo struct {
	Version  string `json:"version"`
	GitCommit string `json:"git_commit"`
}

// isTraceAgentAccessible tests if a Datadog trace agent is accessible by calling the /info endpoint
// Similar to C# TraceAgentAccessible - retries 5 times with 5 second delays
func isTraceAgentAccessible(hostname string, logger *zap.Logger) bool {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	url := fmt.Sprintf("http://%s:8126/info", hostname)
	
	retries := 5
	for retries > 0 {
		resp, err := client.Get(url)
		if err != nil {
			logger.Debug("Failed to connect to Datadog agent", 
				zap.String("host", hostname), 
				zap.String("error", err.Error()),
				zap.Int("retries_left", retries-1))
			retries--
			if retries > 0 {
				time.Sleep(5 * time.Second)
			}
			continue
		}
		
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			logger.Debug("Datadog agent returned non-200 status", 
				zap.String("host", hostname),
				zap.Int("status", resp.StatusCode),
				zap.Int("retries_left", retries-1))
			retries--
			if retries > 0 {
				time.Sleep(5 * time.Second)
			}
			continue
		}
		
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			logger.Debug("Failed to read Datadog agent response", 
				zap.String("host", hostname),
				zap.Error(err),
				zap.Int("retries_left", retries-1))
			retries--
			if retries > 0 {
				time.Sleep(5 * time.Second)
			}
			continue
		}
		
		var info agentInfo
		if err := json.Unmarshal(body, &info); err != nil {
			logger.Debug("Failed to parse Datadog agent info", 
				zap.String("host", hostname),
				zap.Error(err),
				zap.Int("retries_left", retries-1))
			retries--
			if retries > 0 {
				time.Sleep(5 * time.Second)
			}
			continue
		}
		
		// Success - agent is accessible
		logger.Info("Connected to Datadog agent", 
			zap.String("host", hostname),
			zap.String("version", info.Version),
			zap.String("git_commit", info.GitCommit))
		return true
	}
	
	return false
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
		// Skip if DD_NO_AGENT is set (like NetdaemonApp)
		if os.Getenv("DD_NO_AGENT") != "" {
			logger.Info("Datadog tracing skipped (DD_NO_AGENT is set)")
		} else {
			// Support environment variables (DD_AGENT_HOST, DD_TRACE_AGENT_PORT) like NetdaemonApp
			// These take precedence if set
			agentHost := cfg.Datadog.AgentHost
			if envHost := os.Getenv("DD_AGENT_HOST"); envHost != "" {
				agentHost = envHost
				logger.Info("Using DD_AGENT_HOST from environment", zap.String("host", envHost))
			}
			
			agentPort := cfg.Datadog.AgentPort
			if envPort := os.Getenv("DD_TRACE_AGENT_PORT"); envPort != "" {
				if port, err := strconv.Atoi(envPort); err == nil {
					agentPort = port
					logger.Info("Using DD_TRACE_AGENT_PORT from environment", zap.Int("port", port))
				}
			}
			
			// Test connectivity to agent hosts (like NetdaemonApp)
			agentHosts := []string{"127.0.0.1", "local-datadog"}
			if agentHost != "" && agentHost != "localhost" && agentHost != "127.0.0.1" {
				agentHosts = append([]string{agentHost}, agentHosts...)
			}
			
			var accessibleHost string
			for _, host := range agentHosts {
				if isTraceAgentAccessible(host, logger) {
					accessibleHost = host
					break
				}
			}
			
			if accessibleHost == "" {
				logger.Warn("No accessible Datadog Trace Agent found",
					zap.Strings("tried_hosts", agentHosts),
					zap.Int("port", agentPort),
					zap.String("note", "Tracing will be disabled. Check agent connectivity."))
			} else {
				agentAddr := fmt.Sprintf("%s:%d", accessibleHost, agentPort)
				
				// Enable debug logging if log level is debug
				opts := []tracer.StartOption{
					tracer.WithService(cfg.Datadog.ServiceName),
					tracer.WithEnv(cfg.Datadog.Environment),
					tracer.WithAgentAddr(agentAddr),
				}
				
				// Enable debug mode if logging level is debug (helps diagnose connection issues)
				if cfg.Logging.Level == "debug" {
					opts = append(opts, tracer.WithDebugMode(true))
				}
				
				tracer.Start(opts...)
				defer tracer.Stop()
				
				logger.Info("Datadog tracing initialized",
					zap.String("service", cfg.Datadog.ServiceName),
					zap.String("environment", cfg.Datadog.Environment),
					zap.String("agent_addr", agentAddr),
					zap.Bool("debug_mode", cfg.Logging.Level == "debug"),
				)
			}
		}
	}

	logger.Info("Starting EEBUS HEMS")
	logger.Info("Configuration loaded",
		zap.Int("chargers", len(cfg.Chargers)),
		zap.Float64("max_current", cfg.Charging.MaxCurrent),
		zap.Bool("mqtt_enabled", cfg.MQTT.Enabled),
		zap.Bool("datadog_enabled", cfg.Datadog.Enabled),
	)

	// Initialize MQTT handler if enabled
	var mqttHandler *mqtt.MqttHandler
	if cfg.MQTT.Enabled {
		mqttHandler, err = mqtt.NewMqttHandler(
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
		defer mqttHandler.Close()
		logger.Info("MQTT handler initialized",
			zap.String("broker", cfg.MQTT.Broker),
			zap.String("topic_prefix", cfg.MQTT.TopicPrefix),
		)
	}

	// Create EEBUS service
	service, err := eebus.NewService(cfg, logger, mqttHandler)
	if err != nil {
		return fmt.Errorf("failed to create EEBUS service: %w", err)
	}

	// Subscribe to MQTT commands if enabled
	if cfg.MQTT.Enabled && mqttHandler != nil {
		if err := mqttHandler.SubscribeToCommands(service); err != nil {
			return fmt.Errorf("failed to subscribe to MQTT commands: %w", err)
		}
		logger.Info("MQTT command subscription enabled")
	}

	// Start service
	if err := service.Start(); err != nil {
		return fmt.Errorf("failed to start EEBUS service: %w", err)
	}

	// Start API server for control commands
	apiAddr := fmt.Sprintf("localhost:%d", cfg.Network.APIPort)
	apiServer := api.NewServer(service, logger, apiAddr, cfg.Auth)
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

