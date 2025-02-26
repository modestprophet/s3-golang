package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	// Validate user and video ownership
	video, _, err := cfg.validateUserAndVideo(w, r)
	if err != nil {
		return // error already handled
	}

	// Process thumbnail upload
	file, header, err := cfg.processThumbnailUpload(w, r)
	if err != nil {
		return // error already handled
	}
	defer file.Close()

	// Determine and validate file extension
	fileExtension, err := cfg.determineFileExtension(header)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type", nil)
		return
	}

	// Save file to disk
	filePath, err := cfg.saveThumbnailFile(fileExtension, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save thumbnail", err)
		return
	}

	// Update video record
	if err := cfg.updateVideoThumbnail(w, video, filePath); err != nil {
		return // error already handled
	}

	respondWithJSON(w, http.StatusOK, video)
}

// Helper methods:

func (cfg *apiConfig) validateUserAndVideo(w http.ResponseWriter, r *http.Request) (*database.Video, uuid.UUID, error) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return nil, uuid.Nil, err
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return nil, uuid.Nil, err
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return nil, uuid.Nil, err
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return nil, uuid.Nil, err
	}

	//userIDUUID, err := uuid.Parse(userID.String())
	if video.UserID != userID { //userIDUUID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized access", nil)
		return nil, uuid.Nil, fmt.Errorf("unauthorized access")
	}

	return &video, userID, nil
}

func (cfg *apiConfig) processThumbnailUpload(w http.ResponseWriter, r *http.Request) (io.ReadCloser, *multipart.FileHeader, error) {
	const maxMemory = 10 << 20
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return nil, nil, err
	}

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing thumbnail file", err)
		return nil, nil, err
	}
	return file, header, nil
}

func (cfg *apiConfig) determineFileExtension(header *multipart.FileHeader) (string, error) {
	extensions := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
	}

	// Parse media type from Content-Type header
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("invalid Content-Type header: %w", err)
	}

	// Check against allowed types
	if ext, ok := extensions[mediaType]; ok {
		return ext, nil
	}
	return "", fmt.Errorf("unsupported media type: %s", mediaType)
}

func (cfg *apiConfig) saveThumbnailFile(ext string, src io.Reader) (string, error) {
	// Generate 32 random bytes
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	if err != nil {
		return "", fmt.Errorf("could not generate random bytes: %w", err)
	}

	// Encode to URL-safe base64 without padding
	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)
	fileName := randomString + ext
	filePath := filepath.Join(cfg.assetsRoot, fileName)

	dst, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return filePath, nil
}

func (cfg *apiConfig) updateVideoThumbnail(w http.ResponseWriter, video *database.Video, filePath string) error {
	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, filepath.Base(filePath))
	video.ThumbnailURL = &thumbnailURL

	if err := cfg.db.UpdateVideo(*video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return err
	}
	return nil
}

func (cfg *apiConfig) handlerUploadThumbnailMonolith(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// Parse multipart form (10MB max)
	const maxMemory = 10 << 20
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}

	// Get file from form
	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing thumbnail file", err)
		return
	}
	defer file.Close()

	// Get video from database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	// Verify ownership
	userIDUUID, err := uuid.Parse(userID.String())
	if err != nil || video.UserID != userIDUUID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized access", nil)
		return
	}
	// Determine file extension from Content-Type header
	extensions := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		// Add more extensions as needed
	}
	fileExtension, ok := extensions[header.Header.Get("Content-Type")]
	if !ok {
		respondWithError(w, http.StatusBadRequest, "Unsupported file type", nil)
		return
	}

	// Create full path for new file
	filePath := filepath.Join(cfg.assetsRoot, fmt.Sprintf("%s%s", videoID, fileExtension))

	// Create new file
	newFile, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create file", err)
		return
	}
	defer newFile.Close()

	// Copy contents from multipart.File to new file on disk
	_, err = io.Copy(newFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file contents", err)
		return
	}

	// Update thumbnail_url
	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s%s", cfg.port, videoID, fileExtension)
	video.ThumbnailURL = &thumbnailURL

	// Update database record
	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
