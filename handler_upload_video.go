package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Extract and validate video ID from URL path
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Fetch video record from database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't get video", err)
		return
	}

	// Authenticate user via JWT token
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

	// Ensure video belongs to authenticated user
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	// Set max upload size to 10GB and get video file from form
	const maxMemory = 10 << 30
	_ = http.MaxBytesReader(w, r.Body, maxMemory)
	f, h, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse video from file ", err)
		return
	}
	defer f.Close()

	// Validate that uploaded file is MP4
	contentType := h.Header.Get("Content-Type")
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't ParseMediaType", err)
		return
	}
	if mediatype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "mediatype is not valid", nil)
		return
	}

	fileExtension := getFileExtension(mediatype)

	// Create temporary file to store upload
	tf, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tf.Name())
	defer tf.Close()

	// Copy uploaded file to temp file
	_, err = io.Copy(tf, f)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy content of video file to temp file ", err)
		return
	}
	_, err = tf.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Fail to seek temp file back to the beginning", err)
		return
	}

	// Process video for fast start
	fastStartVideoFilePath, err := processVideoForFastStart(tf.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't process video for fast start", err)
		return
	}
	defer os.Remove(fastStartVideoFilePath)

	processedVideo, err := os.Open(fastStartVideoFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't open processed video", err)
		return
	}
	defer processedVideo.Close()

	// Generate random filename for S3
	b := make([]byte, 32)
	_, err = rand.Read(b)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create random bytes", err)
		return
	}
	base64Encoding := base64.RawURLEncoding
	base64Rand := base64Encoding.EncodeToString(b)

	aspectRatio, err := getVideoAspectRatio(tf.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	prefixAspectRatio := getAspectRatioPrefix(aspectRatio)
	// Upload file to S3
	fileKey := fmt.Sprintf("%s/%s.%s", prefixAspectRatio, base64Rand, fileExtension)
	putObjectInput := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		ContentType: &mediatype,
		Body:        processedVideo,
	}
	_, err = cfg.s3Client.PutObject(context.Background(), &putObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Fail to put object to s3 ", err)
		return
	}

	// Update video record with S3 URL
	// videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, fileKey)
	videoURL := strings.Join([]string{cfg.s3Bucket, fileKey}, ",")
	video.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video url", err)
		return
	}

	videoWithSignUrl, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't get db video to signed video", err)
		return
	}

	// Return updated video object
	respondWithJSON(w, http.StatusOK, videoWithSignUrl)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var b bytes.Buffer
	cmd.Stdout = &b
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	var probeData struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	err = json.Unmarshal(b.Bytes(), &probeData)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal ffprobe output: %w", err)
	}

	if len(probeData.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}
	width := probeData.Streams[0].Width
	height := probeData.Streams[0].Height
	// Normalize aspect ratio to common formats
	var normalizedRatio string
	widthFloat := float64(width)
	heightFloat := float64(height)
	ratio := widthFloat / heightFloat

	// Check if ratio matches common formats with some tolerance
	const tolerance = 0.1
	if ratio >= (16.0/9.0)-tolerance && ratio <= (16.0/9.0)+tolerance {
		normalizedRatio = "16:9"
	} else if ratio >= (9.0/16.0)-tolerance && ratio <= (9.0/16.0)+tolerance {
		normalizedRatio = "9:16"
	} else {
		normalizedRatio = "other"
	}
	return normalizedRatio, nil
}

func getAspectRatioPrefix(aspectRatio string) string {
	switch aspectRatio {
	case "16:9":
		return "landscape"
	case "9:16":
		return "portrait"
	default:
		return "other"
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	fastStartVideoFilePath := fmt.Sprintf("%s.processing", filePath)
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", fastStartVideoFilePath)
	err := cmd.Run()
	if err != nil {
		log.Printf("error when running ffmpeg command to processVideoForFastStart: %v\n", err)
		return "", err
	}

	return fastStartVideoFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	v, err := presignClient.PresignGetObject(
		context.Background(),
		&s3.GetObjectInput{Bucket: &bucket, Key: &key},
		s3.WithPresignExpires(expireTime),
	)
	if err != nil {
		return "", err
	}

	return v.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	dbVideoUrl := video.VideoURL
	s := strings.Split(*dbVideoUrl, ",")
	if len(s) != 2 {
		return video, fmt.Errorf("invalid dbVideoUrl, cannot get correct bucket and key: %s", s)
	}
	bucket, key := s[0], s[1]

	duration := 1 * time.Minute
	presignUrl, err := generatePresignedURL(cfg.s3Client, bucket, key, duration)
	if err != nil {
		return video, err
	}

	video.VideoURL = &presignUrl

	return video, nil
}
