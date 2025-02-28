package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Validate user and video ownership
	video, _, err := cfg.validateUserAndVideo(w, r)
	if err != nil {
		return
	}

	// Process video upload
	file, header, err := cfg.processVideoUpload(w, r)
	if err != nil {
		return
	}
	defer file.Close()

	// Validate file type
	if err := cfg.validateVideoType(header); err != nil {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type", err)
		return
	}

	// Create temp file
	tempFile, err := cfg.createTempFile(w)
	if err != nil {
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Save to temp file
	if err := cfg.saveToTempFile(w, file, tempFile); err != nil {
		return
	}

	// Process video for fast start
	processedPath, err := cfg.processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video", err)
		return
	}
	defer os.Remove(processedPath)

	// Open processed file
	processedFile, err := os.Open(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video", err)
		return
	}
	defer processedFile.Close()

	// Determine prefix
	// Get aspect ratio
	prefix, err := cfg.getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't determine aspect ratio", err)
		return
	}

	// Generate S3 key (i.e., random filename)
	key, err := cfg.generateS3Key()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate key", err)
		return
	}

	// pseudo file path
	prefixedKey := prefix + key

	// Upload to S3 with prefixed key
	if err := cfg.uploadToS3(r.Context(), w, processedFile, prefixedKey, header); err != nil {
		return
	}

	// Update video record with prefixed key
	if err := cfg.updateVideoURL(w, video, prefixedKey); err != nil {
		return
	}

	// // Update response to use signed URL
	// signedVideo, err := cfg.dbVideoToSignedVideo(*video)
	// if err != nil {
	// 	if video.VideoURL == nil {
	// 		signedVideo = *video
	// 	} else {
	// 		respondWithError(w, http.StatusInternalServerError, "Failed to generate video URL", err)
	// 		return
	// 	}
	// }

	respondWithJSON(w, http.StatusOK, video)
}

func (cfg *apiConfig) processVideoUpload(w http.ResponseWriter, r *http.Request) (multipart.File, *multipart.FileHeader, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return nil, nil, err
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing video file", err)
		return nil, nil, err
	}
	return file, header, nil
}

func (cfg *apiConfig) validateVideoType(header *multipart.FileHeader) error {
	extensions := map[string]string{
		"video/mp4": ".mp4",
	}

	// Parse media type from Content-Type header
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("invalid Content-Type header: %w", err)
	}

	// Check against allowed types
	if _, ok := extensions[mediaType]; ok {
		return nil
	}
	return fmt.Errorf("unsupported media type: %s", mediaType)
}

func (cfg *apiConfig) createTempFile(w http.ResponseWriter) (*os.File, error) {
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return nil, err
	}
	return tempFile, nil
}

func (cfg *apiConfig) saveToTempFile(w http.ResponseWriter, src io.Reader, dst *os.File) error {
	if _, err := io.Copy(dst, src); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video", err)
		return err
	}

	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return err
	}
	return nil
}

func (cfg *apiConfig) generateS3Key() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("couldn't generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(randomBytes) + ".mp4", nil
}

func (cfg *apiConfig) uploadToS3(ctx context.Context, w http.ResponseWriter, file io.Reader, key string, header *multipart.FileHeader) error {
	contentType := header.Header.Get("Content-Type")
	_, err := cfg.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &key,
		Body:        file,
		ContentType: &contentType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload to S3", err)
		return err
	}
	return nil
}

func (cfg *apiConfig) updateVideoURL(w http.ResponseWriter, video *database.Video, key string) error {
	videoURL := fmt.Sprintf("https://%s/%s", cfg.s3CfDistribution, key)
	video.VideoURL = &videoURL
	if err := cfg.db.UpdateVideo(*video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return err
	}
	return nil
}

func (cfg *apiConfig) getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffprobe error: %w", err)
	}

	var probeOutput struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &probeOutput); err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	if len(probeOutput.Streams) == 0 {
		return "", fmt.Errorf("no streams found in video")
	}

	stream := probeOutput.Streams[0]
	if stream.Width == 0 || stream.Height == 0 {
		return "", fmt.Errorf("invalid video dimensions")
	}

	// Define common aspect ratios with tolerance
	ratio := float64(stream.Width) / float64(stream.Height)
	const tolerance = 0.1

	switch {
	case math.Abs(ratio-16.0/9.0) < tolerance:
		return "landscape/", nil
	case math.Abs(ratio-9.0/16.0) < tolerance:
		return "portrait/", nil
	case math.Abs(ratio-1.0) < tolerance:
		return "square/", nil
	default:
		return "other/", nil
	}
}

func (cfg *apiConfig) processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"
	cmd := exec.Command("ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		outputPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg error: %s: %w", stderr.String(), err)
	}
	return outputPath, nil
}

// func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
// 	presignClient := s3.NewPresignClient(s3Client)

// 	req, err := presignClient.PresignGetObject(context.Background(),
// 		&s3.GetObjectInput{
// 			Bucket: &bucket,
// 			Key:    &key,
// 		},
// 		s3.WithPresignExpires(expireTime),
// 	)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to presign URL: %w", err)
// 	}
// 	return req.URL, nil
// }

// func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
// 	if video.VideoURL == nil {
// 		return video, fmt.Errorf("video URL is nil")
// 	}

// 	// parts[0] = bucket; parts[1] = key/filepath
// 	parts := strings.Split(*video.VideoURL, ",")
// 	if len(parts) != 2 {
// 		return video, fmt.Errorf("invalid video URL format")
// 	}

// 	presignedURL, err := generatePresignedURL(cfg.s3Client, parts[0], parts[1], 24*time.Hour)
// 	if err != nil {
// 		return video, fmt.Errorf("failed to generate presigned URL: %w", err)
// 	}

// 	video.VideoURL = &presignedURL
// 	return video, nil
// }
