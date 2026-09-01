package livedemo

import (
	"context"
	"net/http"
	"strings"

	"github.com/adsrx222/SpectralSpy/src"
	"github.com/gin-gonic/gin"
)

type IdentifyResponse struct {
	SpectralSpy.IdentifyResponse
	Artist    string `json:"artist,omitempty"`
	SongTitle string `json:"song_title,omitempty"`
}

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