package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julienar/eebus-charged/internal/config"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestBasicAuthMiddleware(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name           string
		authConfig     config.AuthConfig
		username       string
		password       string
		expectedStatus int
	}{
		{
			name: "Auth Disabled",
			authConfig: config.AuthConfig{
				Enabled: false,
			},
			expectedStatus: http.StatusOK,
		},
		{
			name: "Auth Enabled - No Credentials",
			authConfig: config.AuthConfig{
				Enabled:  true,
				Username: "admin",
				Password: "password",
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "Auth Enabled - Wrong Password",
			authConfig: config.AuthConfig{
				Enabled:  true,
				Username: "admin",
				Password: "password",
			},
			username:       "admin",
			password:       "wrong",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "Auth Enabled - Wrong Username",
			authConfig: config.AuthConfig{
				Enabled:  true,
				Username: "admin",
				Password: "password",
			},
			username:       "wrong",
			password:       "password",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "Auth Enabled - Correct Credentials",
			authConfig: config.AuthConfig{
				Enabled:  true,
				Username: "admin",
				Password: "password",
			},
			username:       "admin",
			password:       "password",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(nil, logger, ":8080", tt.authConfig)

			// Create a dummy handler that returns 200 OK
			dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			var handler http.Handler = dummyHandler
			if tt.authConfig.Enabled {
				handler = server.basicAuthMiddleware(dummyHandler)
			}

			req := httptest.NewRequest("GET", "/", nil)
			if tt.username != "" || tt.password != "" {
				req.SetBasicAuth(tt.username, tt.password)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			assert.Equal(t, tt.expectedStatus, rec.Code)
		})
	}
}
