package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestIPRateLimiter(t *testing.T) {
	limiter := server.NewIPRateLimiter(rate.Limit(1), 1)
	ip := "192.168.1.1"

	l := limiter.GetLimiter(ip)
	if !l.Allow() {
		t.Errorf("Expected first request from IP to be allowed")
	}

	if l.Allow() {
		t.Errorf("Expected immediate second request to be rate-limited")
	}
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

func TestHandleGetMIDI_CompleteRedirect(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.GET("/songs/:id/midi", app.HandleGetMIDI)

	req := httptest.NewRequest("GET", "/songs/songA/midi", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("Expected status 302 Found, got %d", rr.Code)
	}

	locationHeader := rr.Header().Get("Location")
	if locationHeader == "" {
		t.Fatal("Expected Location header in redirect response")
	}

	parsedURL, err := url.Parse(locationHeader)
	if err != nil {
		t.Fatalf("Invalid Location URL: %v", err)
	}

	queryParams := parsedURL.Query()
	requiredParams := []string{
		"X-Amz-Algorithm",
		"X-Amz-Credential",
		"X-Amz-Date",
		"X-Amz-Expires",
		"X-Amz-SignedHeaders",
		"X-Amz-Signature",
	}
	for _, param := range requiredParams {
		if queryParams.Get(param) == "" {
			t.Errorf("Presigned URL missing query parameter: %s", param)
		}
	}

	s3Resp, err := http.Get(locationHeader)
	if err != nil {
		t.Fatalf("Failed to execute GET request on Location URL: %v", err)
	}
	defer s3Resp.Body.Close()

	if s3Resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected S3 server to respond 200 OK, got %d", s3Resp.StatusCode)
	}

	body, err := io.ReadAll(s3Resp.Body)
	if err != nil {
		t.Fatalf("Failed to read S3 asset body: %v", err)
	}

	if string(body) != "mock-midi-binary-data" {
		t.Errorf("Expected body 'mock-midi-binary-data', got '%s'", string(body))
	}
}

func TestHandleGetConstellation_CompleteRedirect(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.GET("/songs/:id/constellation", app.HandleGetConstellation)

	apiServer := httptest.NewServer(r)
	defer apiServer.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't automatically follow redirect in client to test the 302/200 flow properly
		},
	}

	resp, err := client.Get(apiServer.URL + "/songs/songA/constellation")
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Since we captured last response or followed it, let's verify redirect behavior or query the S3 mock directly if needed.
	// Alternatively, hit the server without the redirect client interceptor if following through:
	apiClientNormal := http.DefaultClient
	respNormal, err := apiClientNormal.Get(apiServer.URL + "/songs/songA/constellation")
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer respNormal.Body.Close()

	if respNormal.StatusCode != http.StatusOK {
		t.Fatalf("Expected final status 200 OK after following redirect, got %d", respNormal.StatusCode)
	}

	body, err := io.ReadAll(respNormal.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}

	if string(body) != "mock-msgpack-binary-data" {
		t.Errorf("Expected body 'mock-msgpack-binary-data', got '%s'", string(body))
	}
}

func TestRateLimitMiddleware_RemoteAddrWithoutPort(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	app.Limiters = server.NewIPRateLimiter(rate.Limit(1), 1)

	r := gin.New()
	r.Use(app.RateLimitMiddleware())
	r.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/ping", nil)
	req.RemoteAddr = "192.168.1.50"
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected request without port in RemoteAddr to succeed, got %d", rr.Code)
	}
}

func TestLoggerMiddleware(t *testing.T) {
	app, s3Server := setupTestApp(t)
	defer app.DB.Close()
	defer s3Server.Close()

	r := gin.New()
	r.Use(app.LoggerMiddleware())
	r.GET("/test-logger", func(c *gin.Context) {
		c.Status(http.StatusAccepted)
	})

	req := httptest.NewRequest("GET", "/test-logger", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", rr.Code)
	}
}