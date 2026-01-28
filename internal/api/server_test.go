package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestEnsureJSONContentType(t *testing.T) {
	tests := []struct {
		name           string
		contentType    string
		want           bool
		wantStatusCode int
	}{
		{
			name:           "Valid Content-Type",
			contentType:    "application/json",
			want:           true,
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "Valid Content-Type with charset",
			contentType:    "application/json; charset=utf-8",
			want:           true,
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "Missing Content-Type",
			contentType:    "",
			want:           false,
			wantStatusCode: http.StatusUnsupportedMediaType,
		},
		{
			name:           "Invalid Content-Type (text/plain)",
			contentType:    "text/plain",
			want:           false,
			wantStatusCode: http.StatusUnsupportedMediaType,
		},
		{
			name:           "Invalid Content-Type (form)",
			contentType:    "application/x-www-form-urlencoded",
			want:           false,
			wantStatusCode: http.StatusUnsupportedMediaType,
		},
	}

	logger := zap.NewNop()
	server := &Server{logger: logger}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/test", nil)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			w := httptest.NewRecorder()

			got := server.ensureJSONContentType(w, req)
			if got != tt.want {
				t.Errorf("ensureJSONContentType() = %v, want %v", got, tt.want)
			}

			if !tt.want {
				if w.Code != tt.wantStatusCode {
					t.Errorf("ensureJSONContentType() status code = %v, want %v", w.Code, tt.wantStatusCode)
				}
				// Verify JSON response
				if w.Header().Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type header = %v, want application/json", w.Header().Get("Content-Type"))
				}
			}
		})
	}
}
