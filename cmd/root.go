package cmd

import (
	"fmt"
	"os"

	"github.com/julienar/eebus-charged/internal/config"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	cfgFile string
	logger  *zap.Logger
)

// rootCmd represents the base command
var rootCmd = &cobra.Command{
	Use:   "eebus-charged",
	Short: "EEBUS-based Home Energy Management System",
	Long: `A standalone EEBUS HEMS for controlling EV chargers.
	
This application provides direct EEBUS communication with compatible
EV chargers (like Porsche Mobile Charger Connect) without requiring
a full EVCC installation.`,
}

// Execute executes the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./config.yaml)")
}

func initConfig() {
	// Logger will be created when needed with config
}

func getLogger() *zap.Logger {
	if logger == nil {
		// Create a basic logger for early startup
		logger, _ = zap.NewDevelopment()
	}
	return logger
}

// CreateLoggerFromConfig creates a logger from configuration
func CreateLoggerFromConfig(logCfg config.LoggingConfig) (*zap.Logger, error) {
	// Parse log level
	var level zapcore.Level
	if err := level.UnmarshalText([]byte(logCfg.Level)); err != nil {
		level = zapcore.InfoLevel
	}

	// Create logger configuration
	var zapConfig zap.Config
	if logCfg.Format == "json" {
		zapConfig = zap.NewProductionConfig()
	} else {
		zapConfig = zap.NewDevelopmentConfig()
		zapConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	zapConfig.Level = zap.NewAtomicLevelAt(level)

	// If log file is specified, log ONLY to file
	if logCfg.File != "" {
		zapConfig.OutputPaths = []string{logCfg.File}
		zapConfig.ErrorOutputPaths = []string{logCfg.File}
	}

	return zapConfig.Build()
}

