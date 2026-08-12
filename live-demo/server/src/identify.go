package livedemo

import (
	"context"
	"net/http"
	"strings"

	"github.com/adsrx222/SpectralSpy/src"
	"github.com/gin-gonic/gin"
)

// IdentifyResponse extends the core server.IdentifyResponse with
// livedemo-specific song metadata. This lives here, not in the server
// package, because the `songs` table (composer/title lookup) is a livedemo
// concept — the core matching schema (audio_hashes/hash_weight) has no
// notion of song titles or composers, and other consumers of server.Identify
// may not have (or want) a songs table at all.
type IdentifyResponse struct {
	SpectralSpy.IdentifyResponse
	Artist    string `json:"artist,omitempty"`
	SongTitle string `json:"song_title,omitempty"`
}

// POST /api/v1/identify
//
// Supports an optional ?include=artist,song query parameter. When present,
// the response is augmented with composer/title looked up from the songs
// table, in addition to the base identify result. A failed or missing
// lookup is non-fatal — the core match result still returns successfully.
//
// Requires a `songs` table shaped like:
//
//	CREATE TABLE songs (
//	    song_id  TEXT PRIMARY KEY,
//	    title    TEXT,
//	    composer TEXT
//	);
//
// (matching the schema the ingestion pipeline's upsert_song step assumes)
func (a *App) HandleIdentify(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*1024*1024)

	var req SpectralSpy.IdentifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "Invalid JSON payload")
		return
	}

	coreResp, err := SpectralSpy.Identify(c.Request.Context(), a.DB, a.Logger, req.Fingerprints)
	if err != nil {
		SpectralSpy.RespondIdentifyError(c, err)
		return
	}

	resp := IdentifyResponse{IdentifyResponse: coreResp}

	// ── ?include=artist,song ────────────────────────────────────────────
	includeSet := parseInclude(c.Query("include"))
	if includeSet["artist"] || includeSet["song"] {
		composer, title, err := a.fetchSongMetadata(c.Request.Context(), coreResp.SongID)
		if err != nil {
			a.Logger.Warn("Failed to fetch song metadata for include",
				"song_id", coreResp.SongID, "err", err)
		} else {
			if includeSet["artist"] {
				resp.Artist = composer
			}
			if includeSet["song"] {
				resp.SongTitle = title
			}
		}
	}

	respondJSON(c, http.StatusOK, resp)
}

// parseInclude splits a comma-separated ?include= value into a lookup set,
// e.g. "artist,song" -> {"artist": true, "song": true}. Unrecognized values
// are silently ignored rather than rejected, keeping the API forward-compatible.
func parseInclude(raw string) map[string]bool {
	set := make(map[string]bool)
	if raw == "" {
		return set
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			set[part] = true
		}
	}
	return set
}

func (a *App) fetchSongMetadata(ctx context.Context, songID string) (composer, title string, err error) {
	err = a.DB.QueryRowContext(ctx,
		`SELECT composer, title FROM songs WHERE song_id = ?`, songID,
	).Scan(&composer, &title)
	return composer, title, err
}