package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"crypto/subtle"

	"github.com/julienar/eebus-charged/internal/config"
	"github.com/julienar/eebus-charged/internal/eebus"
	"go.uber.org/zap"
	httptrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/net/http"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// Server provides HTTP API for controlling the HEMS
type Server struct {
	service *eebus.Service
	logger  *zap.Logger
	addr    string
	auth    config.AuthConfig
}

// NewServer creates a new API server
func NewServer(service *eebus.Service, logger *zap.Logger, addr string, auth config.AuthConfig) *Server {
	return &Server{
		service: service,
		logger:  logger,
		addr:    addr,
		auth:    auth,
	}
}

// Start starts the HTTP server
func (s *Server) Start() error {
	// Use Datadog HTTP tracing middleware
	mux := httptrace.NewServeMux()
	mux.HandleFunc("/api/chargers", s.listChargers)
	mux.HandleFunc("/api/chargers/", s.handleCharger)

	var handler http.Handler = mux

	// Add Basic Auth middleware if enabled
	if s.auth.Enabled {
		handler = s.basicAuthMiddleware(handler)
		s.logger.Info("API Authentication enabled")
	}

	s.logger.Info("Starting API server", zap.String("addr", s.addr))
	return http.ListenAndServe(s.addr, handler)
}

// basicAuthMiddleware enforces Basic Authentication
func (s *Server) basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()

		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(s.auth.Username)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(s.auth.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			s.writeError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Response types
type ErrorResponse struct {
	Error string `json:"error"`
}

type StatusResponse struct {
	Name                 string  `json:"name"`
	SKI                  string  `json:"ski"`
	Connected            bool    `json:"connected"`
	ChargingState        string  `json:"charging_state"`
	CurrentLimit         float64 `json:"current_limit"`
	MinCurrent           float64 `json:"min_current"`
	MaxCurrent           float64 `json:"max_current"`
	VehicleID            string  `json:"vehicle_id,omitempty"`
	VehicleName          string  `json:"vehicle_name,omitempty"`
	CommunicationStandard string            `json:"communication_standard,omitempty"`
	ControllerType       string            `json:"controller_type,omitempty"`
	Features             map[string]map[string]interface{} `json:"features,omitempty"` // Feature name -> properties (e.g., {"supported": true, "limits": {...}})
}

type SetCurrentRequest struct {
	Current float64 `json:"current"`
}

type SuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// listChargers returns all chargers
func (s *Server) listChargers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	chargers := s.service.ListChargers()
	statuses := make([]StatusResponse, 0, len(chargers))

	for _, c := range chargers {
		status := s.chargerToStatus(c)
		statuses = append(statuses, status)
	}

	s.writeJSON(w, http.StatusOK, statuses)
}

// handleCharger handles charger-specific operations
func (s *Server) handleCharger(w http.ResponseWriter, r *http.Request) {
	span, _ := tracer.StartSpanFromContext(r.Context(), "api.handle_charger")
	defer span.Finish()

	// Extract charger name from path: /api/chargers/{name}/{action}
	path := r.URL.Path[len("/api/chargers/"):]
	
	// Parse path by splitting on /
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) == 0 || parts[0] == "" {
		s.writeError(w, http.StatusBadRequest, "charger name required")
		return
	}

	chargerName := parts[0]
	span.SetTag("charger", chargerName)

	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	span.SetTag("action", action)

	// Get charger
	charger, err := s.service.GetCharger(chargerName)
	if err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Handle actions
	switch action {
	case "status":
		if r.Method != http.MethodGet {
			s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		status := s.chargerToStatus(charger)
		s.writeJSON(w, http.StatusOK, status)

	case "start":
		if r.Method != http.MethodPost {
			s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := charger.StartCharging(); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Charging started",
		})

	case "stop":
		if r.Method != http.MethodPost {
			s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := charger.StopCharging(); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Charging stopped",
		})

	case "current":
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		
		var req SetCurrentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
			return
		}

		// Validate current value
		if req.Current < 0 {
			s.writeError(w, http.StatusBadRequest, "current must be non-negative")
			return
		}

		if err := charger.SetCurrentLimit(req.Current); err != nil {
			// Return 400 for validation errors, 500 for other errors
			if strings.Contains(err.Error(), "must be") || strings.Contains(err.Error(), "above") || strings.Contains(err.Error(), "below") {
				s.writeError(w, http.StatusBadRequest, err.Error())
			} else {
				s.writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		s.writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: fmt.Sprintf("Current limit set to %.1fA", req.Current),
		})

	default:
		// If no action, return status
		if action == "" && r.Method == http.MethodGet {
			status := s.chargerToStatus(charger)
			s.writeJSON(w, http.StatusOK, status)
			return
		}
		s.writeError(w, http.StatusNotFound, "unknown action")
	}
}

// chargerToStatus converts charger to status response
func (s *Server) chargerToStatus(charger *eebus.Charger) StatusResponse {
	status := charger.GetStatus()
	
	resp := StatusResponse{
		Name:          status["name"].(string),
		SKI:           status["ski"].(string),
		Connected:     status["connected"].(bool),
		ChargingState: status["charging_state"].(string),
		CurrentLimit:  status["current_limit"].(float64),
		MinCurrent:    status["min_current"].(float64),
		MaxCurrent:    status["max_current"].(float64),
		VehicleID:     getStringOrEmpty(status, "vehicle_id"),
		VehicleName:   getStringOrEmpty(status, "vehicle_name"),
	}
	
	// Add capabilities if available
	if commStd, ok := status["communication_standard"].(string); ok {
		resp.CommunicationStandard = commStd
	}
	if ctrlType, ok := status["controller_type"].(string); ok {
		resp.ControllerType = ctrlType
	}
	
	// Extract features from status (built in charger.go)
	if features, ok := status["features"].(map[string]map[string]interface{}); ok {
		resp.Features = features
	}
	
	return resp
}

// Helper functions
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.logger.Warn("API error", zap.String("error", message), zap.Int("status", status))
	s.writeJSON(w, status, ErrorResponse{Error: message})
}

func getStringOrEmpty(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

