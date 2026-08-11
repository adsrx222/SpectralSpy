package SpectralSpy

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

func setupTestRouter(t *testing.T) (*gin.Engine, *sql.DB) {
	t.Helper()

	// SetupTestDB is assumed to be in the same package (formerly testutil.go)
	dbConn := setupTestDB(t)

	seeds := []string{
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (101, 'songA', 4096.0)",
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (102, 'songA', 4196.0)",
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (103, 'songB', 8192.0)",
		"INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (-1, 'songLargeHash', 2048.0)",
	}

	for _, seed := range seeds {
		if _, err := dbConn.Exec(seed); err != nil {
			t.Fatalf("Failed to seed database with query [%s]: %v", seed, err)
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	r := gin.New()
	r.POST("/api/v1/identify", NewIdentifyHandler(dbConn, logger))

	return r, dbConn
}

func TestHandleIdentify_Success(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	reqPayload := IdentifyRequest{
		Fingerprints: []Fingerprint{
			{Hash: 101, AnchorTime: 0.0},
			{Hash: 102, AnchorTime: 100.0},
		},
	}

	body, _ := json.Marshal(reqPayload)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp IdentifyResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response JSON: %v", err)
	}

	if resp.SongID != "songA" {
		t.Errorf("Expected SongID 'songA', got '%s'", resp.SongID)
	}
    
	// Check if the offset is within a reasonable tolerance
	expectedOffset := 0.092880
	tolerance := 0.001
	if math.Abs(resp.TimeOffset-expectedOffset) > tolerance {
		t.Errorf("Expected TimeOffset within %f of %f, got %f", tolerance, expectedOffset, resp.TimeOffset)
	}
}

func TestHandleIdentify_ValidationErrors(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	tests := []struct {
		name         string
		payload      IdentifyRequest
		expectedCode int
	}{
		{
			name:         "Empty Fingerprints",
			payload:      IdentifyRequest{Fingerprints: []Fingerprint{}},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Exceeds Fingerprint Limit",
			payload:      IdentifyRequest{Fingerprints: make([]Fingerprint, 500001)},
			expectedCode: http.StatusBadRequest,
		},
		{
			name: "No Match In DB",
			payload: IdentifyRequest{
				Fingerprints: []Fingerprint{{Hash: 99999, AnchorTime: 0}},
			},
			expectedCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.payload)
			req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			r.ServeHTTP(rr, req)

			if rr.Code != tc.expectedCode {
				t.Errorf("Expected code %d, got %d", tc.expectedCode, rr.Code)
			}
		})
	}
}

func TestHandleIdentify_MalformedJSON(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	badJSON := []byte(`{"fingerprints": [{"hash": 101, "anchor_time": }]}`)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(badJSON))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 Bad Request for malformed JSON, got %d", rr.Code)
	}
}

func TestHandleIdentify_ExceedsMaxBytes(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	largePadding := strings.Repeat("a", 110*1024)
	payload := map[string]string{"padding": largePadding}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 Bad Request when exceeding max body bytes, got %d", rr.Code)
	}
}

func TestHandleIdentify_LargeUint64Hash(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	maxHash := uint64(18446744073709551615)
	reqPayload := IdentifyRequest{
		Fingerprints: []Fingerprint{
			{Hash: maxHash, AnchorTime: 0.0},
		},
	}

	body, _ := json.Marshal(reqPayload)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200 for large uint64 hash, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp IdentifyResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.SongID != "songLargeHash" {
		t.Errorf("Expected SongID 'songLargeHash', got '%s'", resp.SongID)
	}
}

func TestHandleIdentify_DuplicateHashes(t *testing.T) {
	r, db := setupTestRouter(t)
	defer db.Close()

	reqPayload := IdentifyRequest{
		Fingerprints: []Fingerprint{
			{Hash: 101, AnchorTime: 0.0},
			{Hash: 101, AnchorTime: 2048.0},
		},
	}

	body, _ := json.Marshal(reqPayload)
	req := httptest.NewRequest("POST", "/api/v1/identify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200 for duplicate hash query, got %d", rr.Code)
	}
}