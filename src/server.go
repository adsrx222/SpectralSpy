package SpectralSpy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	SpectralSpyEngine "github.com/adsrx222/SpectralSpy/src/fp-engine"
	"github.com/gin-gonic/gin"
)

const MAX_REQUEST_LENGTH = 500000

var (
	ErrNoFingerprints      = errors.New("no fingerprints provided")
	ErrTooManyFingerprints = errors.New("too many fingerprints")
	ErrNoMatch             = errors.New("no matching song found")
)

type IdentifyRequest struct {
	Fingerprints []SpectralSpyEngine.Fingerprint `json:"fingerprints"`
}

type Diagnostics struct {
	MatchScore     float64 `json:"match_score"`
	QueryHashes    int     `json:"query_hashes"`
	UniqueDBHashes int     `json:"unique_db_hashes"`
	DecodeTimeMs   int64   `json:"decode_time_ms"`
	ExtractTimeMs  int64   `json:"extract_time_ms"`
	DBQueryTimeMs  int64   `json:"db_query_time_ms"`
	StreamTimeMs   int64   `json:"stream_time_ms"`
	MatchTimeMs    int64   `json:"match_time_ms"`
	TotalTimeMs    int64   `json:"total_time_ms"`
}

type IdentifyResponse struct {
	SongID      string      `json:"song_id"`
	Confidence  float64     `json:"confidence"`
	TimeOffset  float64     `json:"time_offset"`
	Diagnostics Diagnostics `json:"diagnostics"`
}

type APIError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

func Identify(ctx context.Context, db *sql.DB, logger *slog.Logger, fingerprints []SpectralSpyEngine.Fingerprint) (IdentifyResponse, error) {
	if logger == nil {
		logger = slog.Default()
	}

	reqLen := len(fingerprints)
	if reqLen == 0 {
		return IdentifyResponse{}, ErrNoFingerprints
	}
	if reqLen > MAX_REQUEST_LENGTH {
		return IdentifyResponse{}, fmt.Errorf("%w: maximum %d fingerprints allowed", ErrTooManyFingerprints, MAX_REQUEST_LENGTH)
	}

	t1 := time.Now()
	uniqueHashes := make(map[uint64]bool, reqLen)
	for _, fp := range fingerprints {
		uniqueHashes[fp.Hash] = true
	}
	extractDuration := time.Since(t1)

	t2 := time.Now()
	placeholders := make([]string, 0, len(uniqueHashes))
	args := make([]interface{}, 0, len(uniqueHashes))

	for hash := range uniqueHashes {
		placeholders = append(placeholders, "?")
		args = append(args, int64(hash))
	}

	query := fmt.Sprintf(`
		SELECT ah.hash, ah.song_id, ah.anchor_time, COALESCE(hw.weight, 1.0) AS weight
		FROM audio_hashes ah
		LEFT JOIN hash_weight hw ON ah.hash = hw.hash
		WHERE ah.hash IN (%s)`, strings.Join(placeholders, ","))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		logger.Error("Database query failed", "err", err)
		return IdentifyResponse{}, fmt.Errorf("database query failed: %w", err)
	}
	defer rows.Close()
	dbQueryDuration := time.Since(t2)

	t3 := time.Now()
	dbMap := make(map[uint64][]SpectralSpyEngine.DBEntry)

	for rows.Next() {
		var hash int64
		var songID string
		var anchorTime float64
		var weight float64

		if err := rows.Scan(&hash, &songID, &anchorTime, &weight); err != nil {
			logger.Warn("Failed to scan row", "err", err)
			continue
		}

		uintHash := uint64(hash)
		dbMap[uintHash] = append(dbMap[uintHash], SpectralSpyEngine.DBEntry{
			Hash:       uintHash,
			SongID:     songID,
			AnchorTime: anchorTime,
			Weight:     weight,
		})
	}

	if err := rows.Err(); err != nil {
		logger.Error("Row iteration error", "err", err)
		return IdentifyResponse{}, fmt.Errorf("database read error: %w", err)
	}
	streamDuration := time.Since(t3)

	if len(dbMap) == 0 {
		return IdentifyResponse{}, ErrNoMatch
	}

	t4 := time.Now()
	bestSongID, bestScore, matchOffset, confidence := SpectralSpyEngine.MatchFingerprints(fingerprints, dbMap)
	if bestSongID == "" {
		return IdentifyResponse{}, ErrNoMatch
	}
	matchDuration := time.Since(t4)

	logger.Info("Identify completed",
		"song_id", bestSongID,
		"match_score", bestScore,
		"match_offset_sec", fmt.Sprintf("%.2f", matchOffset),
		"confidence", fmt.Sprintf("%.3f", confidence),
		"query_hashes", reqLen,
		"unique_db_hashes", len(dbMap),
		"timing_extract_ms", extractDuration.Milliseconds(),
		"timing_db_query_ms", dbQueryDuration.Milliseconds(),
		"timing_stream_ms", streamDuration.Milliseconds(),
		"timing_match_ms", matchDuration.Milliseconds(),
	)

	return IdentifyResponse{
		SongID:     bestSongID,
		Confidence: confidence,
		TimeOffset: matchOffset,
		Diagnostics: Diagnostics{
			MatchScore:     bestScore,
			QueryHashes:    reqLen,
			UniqueDBHashes: len(dbMap),
			ExtractTimeMs:  extractDuration.Milliseconds(),
			DBQueryTimeMs:  dbQueryDuration.Milliseconds(),
			StreamTimeMs:   streamDuration.Milliseconds(),
			MatchTimeMs:    matchDuration.Milliseconds(),
		},
	}, nil
}

// thin HTTP wrapper around Identify() with no extra behavior
func NewIdentifyHandler(db *sql.DB, logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}

	return func(c *gin.Context) {
		t0 := time.Now()
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*1024*1024)

		var req IdentifyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			respondError(c, http.StatusBadRequest, "Invalid JSON payload")
			return
		}
		decodeDuration := time.Since(t0)

		resp, err := Identify(c.Request.Context(), db, logger, req.Fingerprints)
		if err != nil {
			RespondIdentifyError(c, err)
			return
		}

		resp.Diagnostics.DecodeTimeMs = decodeDuration.Milliseconds()
		resp.Diagnostics.TotalTimeMs = time.Since(t0).Milliseconds()

		respondJSON(c, http.StatusOK, resp)
	}
}

func RespondIdentifyError(c *gin.Context, err error) {
	switch {
		case errors.Is(err, ErrNoFingerprints):
			respondError(c, http.StatusBadRequest, "No fingerprints provided")
		case errors.Is(err, ErrTooManyFingerprints):
			respondError(c, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNoMatch):
			respondError(c, http.StatusNotFound, "No matching song found")
		default:
			respondError(c, http.StatusInternalServerError, "Internal server error")
	}
}

func respondJSON(c *gin.Context, status int, data interface{}) {
	c.JSON(status, data)
}

func respondError(c *gin.Context, status int, message string) {
	c.JSON(status, APIError{Error: message})
}