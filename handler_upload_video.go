package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not this video's owner", nil)
		return
	}

	f, fh, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file", err)
		return
	}
	defer f.Close()
	mediaType, _, err := mime.ParseMediaType(fh.Header.Get("Content-Type"))
	if err != nil || mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Wrong mime type, expected video/mp4, got "+mediaType, err)
		return
	}
	ext := strings.Split(mediaType, "/")[1]

	tmp, err := os.CreateTemp("/tmp/", `tubely-upload.mp4-*`)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create tmp file", err)
		return
	}
	defer os.Remove(tmp.Name())
	log.Println("created tmp file: ", tmp.Name())
	defer tmp.Close()

	reader := http.MaxBytesReader(w, f, 1<<30)
	defer reader.Close()

	_, err = io.Copy(tmp, reader)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy video file", err)
		return
	}

	prefix, err := getVideoAspectRatio(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Missing data on video's aspect ratio", err)
		return
	}

	// _, err = tmp.Seek(0, io.SeekStart)
	// if err != nil {
	// 	respondWithError(w, http.StatusInternalServerError, "Couldn't rewind tmp file", err)
	// 	return
	// }

	filePath, err := processVideoForFastStart(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for faststart", err)
		return
	}

	processed, err := os.Open(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video", err)
	}
	defer os.Remove(processed.Name())
	defer processed.Close()

	b := make([]byte, 32)
	_, _ = rand.Read(b)
	videoKey := fmt.Sprintf("%s/%s.%s",
		prefix,
		base64.RawURLEncoding.EncodeToString(b),
		ext,
	)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &videoKey,
		Body:        processed,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}

	region := cfg.s3Client.Options().Region
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s",
		"tubely-3773",
		region,
		videoKey,
	)
	video.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't udpate video metadata", err)
		return
	}
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams", filePath,
	)
	buf := bytes.NewBuffer(nil)
	cmd.Stdout = buf
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("couldn't run command: %v", err)
	}

	type outputFormat = struct {
		Streams []struct {
			DisplayAspectRatio *string `json:"display_aspect_ratio"`
		}
	}
	var output outputFormat
	err = json.Unmarshal(buf.Bytes(), &output)
	if err != nil {
		return "", fmt.Errorf("couldn't parse command's output: %v", err)
	}
	if len(output.Streams) == 0 {
		return "", errors.New("empty command's output")
	}

	aspectRatio := output.Streams[0].DisplayAspectRatio
	if aspectRatio == nil {
		return "", errors.New("couldn't get aspect ratio from command's output")
	}
	switch *aspectRatio {
	case "16:9":
		return "landscape", nil
	case "9:16":
		return "portrait", nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filepath string) (string, error) {
	fp := filepath + ".processing"
	cmd := exec.Command(
		"ffmpeg",
		"-i", filepath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		fp,
	)
	cmd.Stdout = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("couldn't run command: %v", err)
	}

	return fp, nil
}
