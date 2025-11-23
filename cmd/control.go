package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	chargerName string
	current     float64
	apiAddr     string
)

const defaultAPIAddr = "http://localhost:8080"

// API response types
type apiStatusResponse struct {
	Name          string  `json:"name"`
	SKI           string  `json:"ski"`
	Connected     bool    `json:"connected"`
	ChargingState string  `json:"charging_state"`
	CurrentLimit  float64 `json:"current_limit"`
	MinCurrent    float64 `json:"min_current"`
	MaxCurrent    float64 `json:"max_current"`
	VehicleID     string  `json:"vehicle_id,omitempty"`
	VehicleName   string  `json:"vehicle_name,omitempty"`
}

type apiSuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type apiErrorResponse struct {
	Error string `json:"error"`
}

var controlCmd = &cobra.Command{
	Use:   "control",
	Short: "Control charger operations",
	Long:  `Send control commands to connected chargers.`,
}

var statusCmd = &cobra.Command{
	Use:   "status [charger-name]",
	Short: "Get charger status",
	Long:  `Display the current status of a charger or all chargers.`,
	RunE:  getStatus,
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start charging",
	Long:  `Start the charging process on the specified charger.`,
	RunE:  startCharging,
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop charging",
	Long:  `Stop the charging process on the specified charger.`,
	RunE:  stopCharging,
}

var setCurrentCmd = &cobra.Command{
	Use:   "set-current",
	Short: "Set charging current limit",
	Long:  `Set the maximum charging current limit in Amperes.`,
	RunE:  setCurrentLimit,
}

func init() {
	rootCmd.AddCommand(controlCmd)

	controlCmd.AddCommand(statusCmd)
	controlCmd.AddCommand(startCmd)
	controlCmd.AddCommand(stopCmd)
	controlCmd.AddCommand(setCurrentCmd)

	// Add global API address flag
	controlCmd.PersistentFlags().StringVar(&apiAddr, "api", defaultAPIAddr, "API server address")

	// Add flags
	statusCmd.Flags().StringVarP(&chargerName, "charger", "c", "", "charger name (optional, shows all if not specified)")
	
	startCmd.Flags().StringVarP(&chargerName, "charger", "c", "", "charger name (required)")
	startCmd.MarkFlagRequired("charger")

	stopCmd.Flags().StringVarP(&chargerName, "charger", "c", "", "charger name (required)")
	stopCmd.MarkFlagRequired("charger")

	setCurrentCmd.Flags().StringVarP(&chargerName, "charger", "c", "", "charger name (required)")
	setCurrentCmd.Flags().Float64VarP(&current, "current", "a", 0, "current limit in Amperes (required)")
	setCurrentCmd.MarkFlagRequired("charger")
	setCurrentCmd.MarkFlagRequired("current")
}

func getStatus(cmd *cobra.Command, args []string) error {
	var url string
	if chargerName != "" {
		url = fmt.Sprintf("%s/api/chargers/%s/status", apiAddr, chargerName)
	} else {
		url = fmt.Sprintf("%s/api/chargers", apiAddr)
	}

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w\nMake sure the service is running with: eebus-charged run", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp apiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return fmt.Errorf("API error: %s", errResp.Error)
		}
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCONNECTED\tVEHICLE\tCURRENT\tSTATE")
	fmt.Fprintln(w, "----\t---------\t-------\t-------\t-----")

	if chargerName != "" {
		// Single charger
		var status apiStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
		printChargerStatus(w, status)
	} else {
		// All chargers
		var statuses []apiStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
		for _, status := range statuses {
			printChargerStatus(w, status)
		}
	}

	w.Flush()
	return nil
}

func printChargerStatus(w *tabwriter.Writer, status apiStatusResponse) {
	connected := "No"
	if status.Connected {
		connected = "Yes"
	}

	vehicle := "-"
	if status.VehicleID != "" {
		if status.VehicleName != "" {
			vehicle = fmt.Sprintf("%s (%s)", status.VehicleName, status.VehicleID)
		} else {
			vehicle = status.VehicleID
		}
	}

	fmt.Fprintf(w, "%s\t%s\t%s\t%.1fA\t%s\n",
		status.Name,
		connected,
		vehicle,
		status.CurrentLimit,
		status.ChargingState,
	)
}

func startCharging(cmd *cobra.Command, args []string) error {
	url := fmt.Sprintf("%s/api/chargers/%s/start", apiAddr, chargerName)
	
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w\nMake sure the service is running with: eebus-charged run", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp apiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return fmt.Errorf("API error: %s", errResp.Error)
		}
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var successResp apiSuccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&successResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	fmt.Printf("✓ %s\n", successResp.Message)
	return nil
}

func stopCharging(cmd *cobra.Command, args []string) error {
	url := fmt.Sprintf("%s/api/chargers/%s/stop", apiAddr, chargerName)
	
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w\nMake sure the service is running with: eebus-charged run", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp apiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return fmt.Errorf("API error: %s", errResp.Error)
		}
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var successResp apiSuccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&successResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	fmt.Printf("✓ %s\n", successResp.Message)
	return nil
}


func setCurrentLimit(cmd *cobra.Command, args []string) error {
	url := fmt.Sprintf("%s/api/chargers/%s/current", apiAddr, chargerName)
	
	reqBody := map[string]float64{"current": current}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w\nMake sure the service is running with: eebus-charged run", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp apiErrorResponse
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &errResp); err == nil {
			return fmt.Errorf("API error: %s", errResp.Error)
		}
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var successResp apiSuccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&successResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	fmt.Printf("✓ %s\n", successResp.Message)
	return nil
}

