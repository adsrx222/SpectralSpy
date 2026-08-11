package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	_ "github.com/mattn/go-sqlite3"

	"github.com/adsrx222/SpectralSpy/SpectralSpy"
	"github.com/adsrx222/SpectralSpy/server"
	"github.com/adsrx222/SpectralSpy/testutil"
)

func setupTestApp(t *testing.T) (*server.App, *httptest.Server) {
	t.Helper()

	dbConn := testutil.SetupTestDB(t, "")

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

	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		queryParams := r.URL.Query()
		if queryParams.Get("X-Amz-Algorithm") == "" || queryParams.Get("X-Amz-Signature") == "" {
			http.Error(w, "Forbidden: Missing AWS S3 presigned parameters", http.StatusForbidden)
			return
		}

		switch {
		case strings.HasSuffix(r.URL.Path, "/test-bucket/midi/songA.mid"):
			w.Header().Set("Content-Type", "audio/midi")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("mock-midi-binary-data"))

		case strings.HasSuffix(r.URL.Path, "/test-bucket/peaks/cm_songA.msgpack"):
			w.Header().Set("Content-Type", "application/msgpack")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("mock-msgpack-binary-data"))

		default:
			http.Error(w, "Not Found in S3", http.StatusNotFound)
		}
	}))

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-key", "test-secret", "")),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Server.URL)
		o.UsePathStyle = true
	})

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	app := server.NewApp(dbConn, s3Client, "test-bucket", logger)
	app.Limiters = server.NewIPRateLimiter(rate.Limit(10), 5)

	return app, s3Server
}

func TestHandleIdentify_Success(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.POST("/api/v1/identify", app.HandleIdentify)

	reqPayload := server.IdentifyRequest{
		Fingerprints: []SpectralSpy.Fingerprint{
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

	var resp server.IdentifyResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response JSON: %v", err)
	}

	if resp.SongID != "songA" {
		t.Errorf("Expected SongID 'songA', got '%s'", resp.SongID)
	}
	if resp.TimeOffset != 2 {
		t.Errorf("Expected TimeOffset 2, got %f", resp.TimeOffset)
	}
}

func TestHandleIdentify_ValidationErrors(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.POST("/api/v1/identify", app.HandleIdentify)

	tests := []struct {
		name         string
		payload      server.IdentifyRequest
		expectedCode int
	}{
		{
			name:         "Empty Fingerprints",
			payload:      server.IdentifyRequest{Fingerprints: []SpectralSpy.Fingerprint{}},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Exceeds Fingerprint Limit",
			payload:      server.IdentifyRequest{Fingerprints: make([]SpectralSpy.Fingerprint, 500001)},
			expectedCode: http.StatusBadRequest,
		},
		{
			name: "No Match In DB",
			payload: server.IdentifyRequest{
				Fingerprints: []SpectralSpy.Fingerprint{{Hash: 99999, AnchorTime: 0}},
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
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.POST("/api/v1/identify", app.HandleIdentify)

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
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.POST("/api/v1/identify", app.HandleIdentify)

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
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.POST("/api/v1/identify", app.HandleIdentify)

	maxHash := uint64(18446744073709551615)
	reqPayload := server.IdentifyRequest{
		Fingerprints: []SpectralSpy.Fingerprint{
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

	var resp server.IdentifyResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.SongID != "songLargeHash" {
		t.Errorf("Expected SongID 'songLargeHash', got '%s'", resp.SongID)
	}
}

func TestHandleIdentify_DuplicateHashes(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.POST("/api/v1/identify", app.HandleIdentify)

	reqPayload := server.IdentifyRequest{
		Fingerprints: []SpectralSpy.Fingerprint{
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