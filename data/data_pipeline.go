package data

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"spectralspy/pkg/SpectralSpy"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/zeebo/xxh3"
)

const (
	MaestroURL      = "https://storage.googleapis.com/magentadata/datasets/maestro/v3.0.0/maestro-v3.0.0.zip"
	MaestroMIDI_URL = "https://storage.googleapis.com/magentadata/datasets/maestro/v3.0.0/maestro-v3.0.0-midi.zip"
	MaestroJSON_URL = "https://storage.googleapis.com/magentadata/datasets/maestro/v3.0.0/maestro-v3.0.0.json"
	WorkspaceDir    = "misc/workspace"
)

type SongMetadata struct {
	Artist   string
	Title    string
	Year     int
	MidiPath string
	WavPath  string
}

type MaestroDataframe struct {
	CanonicalComposer map[string]string `json:"canonical_composer"`
	CanonicalTitle    map[string]string `json:"canonical_title"`
	Year              map[string]int    `json:"year"`
	MidiFilename      map[string]string `json:"midi_filename"`
	AudioFilename     map[string]string `json:"audio_filename"`
}

type ProcessWavCallback func(fileName string, meta SongMetadata, r io.Reader, current, total int) error

func GetSongID(wavPath string) string {
	hash64 := xxh3.HashString(wavPath)
	return strconv.FormatUint(hash64, 36)
}

func BatchInsertHashes(ctx context.Context, db *sql.DB, songID string, hashes []SpectralSpy.HashEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO audio_hashes (hash, song_id, anchor_time) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, h := range hashes {
		if _, err := stmt.ExecContext(ctx, int64(h.Hash), songID, h.AnchorTime); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func InitCloudflareR2() (*s3.Client, string, error) {
	accountID := os.Getenv("R2_ACCOUNT_ID")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	bucketName := os.Getenv("R2_BUCKET_NAME")

	if accountID == "" || bucketName == "" {
		return nil, "", fmt.Errorf("missing R2 environment variables")
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, "", err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID))
	})

	return client, bucketName, nil
}

func UploadToR2(ctx context.Context, client *s3.Client, bucket, key string, data []byte, contentType string) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(data))),
	})
	return err
}

func OpenZipReader(path string) (*zip.ReadCloser, error) {
	return zip.OpenReader(path)
}

func ExtractMidiToMemory(zr *zip.ReadCloser, targetMidiPath string) ([]byte, error) {
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, targetMidiPath) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("midi file %s not found in zip", targetMidiPath)
}

func LoadSongMap(workspaceDir string) (map[string]SongMetadata, error) {
	jsonPath := filepath.Join(workspaceDir, "maestro-v3.0.0.json")
	file, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("reading json: %w", err)
	}

	var df MaestroDataframe
	if err := json.Unmarshal(file, &df); err != nil {
		return nil, fmt.Errorf("unmarshaling json dataframe: %w", err)
	}

	songMap := make(map[string]SongMetadata)
	for key, audioPath := range df.AudioFilename {
		songMap[audioPath] = SongMetadata{
			Artist:   df.CanonicalComposer[key],
			Title:    df.CanonicalTitle[key],
			Year:     df.Year[key],
			MidiPath: df.MidiFilename[key],
			WavPath:  audioPath,
		}
	}
	return songMap, nil
}

func ProcessWavFiles(workspaceDir string, songMap map[string]SongMetadata, callback ProcessWavCallback) error {
	audioZipPath := filepath.Join(workspaceDir, "maestro-v3.0.0.zip")
	zr, err := zip.OpenReader(audioZipPath)
	if err != nil {
		return fmt.Errorf("opening audio zip: %w", err)
	}
	defer zr.Close()

	totalSongs := len(songMap)
	currentSong := 1

	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Ext(f.Name), ".wav") {
			continue
		}

		var matchedMeta SongMetadata
		var matched bool
		for key, meta := range songMap {
			if strings.HasSuffix(f.Name, key) {
				matchedMeta = meta
				matched = true
				break
			}
		}

		if !matched {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening wav %s: %w", f.Name, err)
		}

		if err := callback(f.Name, matchedMeta, rc, currentSong, totalSongs); err != nil {
			rc.Close()
			return err
		}
		rc.Close()

		currentSong++
	}

	return nil
}

func DecodeWavToFloat64(r io.Reader) ([]float64, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		rs = bytes.NewReader(b)
	}

	d := wav.NewDecoder(rs)
	if !d.IsValidFile() {
		return nil, fmt.Errorf("invalid WAV stream")
	}

	format := d.Format()
	if format == nil {
		return nil, fmt.Errorf("could not parse WAV format")
	}

	numChannels := format.NumChannels
	bitDepth := d.BitDepth

	var maxVal float64
	switch bitDepth {
	case 8:
		maxVal = 128.0
	case 16:
		maxVal = 32768.0
	case 24:
		maxVal = 8388608.0
	case 32:
		maxVal = 2147483648.0
	default:
		return nil, fmt.Errorf("unsupported bit depth: %d", bitDepth)
	}

	var samples []float64
	buf := &audio.IntBuffer{
		Format:         format,
		Data:           make([]int, 8192*numChannels),
		SourceBitDepth: int(bitDepth),
	}

	for {
		n, err := d.PCMBuffer(buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		for i := 0; i < n; i += numChannels {
			var sum float64
			for c := 0; c < numChannels; c++ {
				sum += float64(buf.Data[i+c]) / maxVal
			}
			samples = append(samples, sum/float64(numChannels))
		}
	}
	return samples, nil
}

func PrepareMaestro(dir string) error {
	status := DetermineMissing(dir)

	if !status[0] {
		if err := downloadFile(MaestroURL, filepath.Join(dir, "maestro-v3.0.0.zip")); err != nil {
			return fmt.Errorf("failed downloading maestro zip: %w", err)
		}
	}
	if !status[1] {
		if err := downloadFile(MaestroJSON_URL, filepath.Join(dir, "maestro-v3.0.0.json")); err != nil {
			return fmt.Errorf("failed downloading maestro json: %w", err)
		}
	}
	if !status[2] {
		if err := downloadFile(MaestroMIDI_URL, filepath.Join(dir, "maestro-v3.0.0-midi.zip")); err != nil {
			return fmt.Errorf("failed downloading maestro midi: %w", err)
		}
	}
	return nil
}

func DetermineMissing(workspaceDir string) []bool {
	results := make([]bool, 3)
	if _, err := os.Stat(filepath.Join(workspaceDir, "maestro-v3.0.0.zip")); err == nil {
		results[0] = true
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "maestro-v3.0.0.json")); err == nil {
		results[1] = true
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "maestro-v3.0.0-midi.zip")); err == nil {
		results[2] = true
	}
	return results
}

func downloadFile(url, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	cmd := exec.Command("curl", "-L", "--fail", "-o", path, url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}