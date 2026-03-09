package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Environment variables
const (
	NCMEDIA_ENV              = "NCMEDIA_ENV"
	NCMEDIA_ADDR             = "NCMEDIA_ADDR"
	NCMEDIA_MINIO_ENDPOINT   = "NCMEDIA_MINIO_ENDPOINT"
	NCMEDIA_MINIO_ACCESS_KEY = "NCMEDIA_MINIO_ACCESS_KEY"
	NCMEDIA_MINIO_SECRET_KEY = "NCMEDIA_MINIO_SECRET_KEY"
	NCMEDIA_MINIO_USE_SSL    = "NCMEDIA_MINIO_USE_SSL"
	NCMEDIA_REQUEST_TIMEOUT  = "NCMEDIA_REQUEST_TIMEOUT"
	NCMEDIA_USERNAME         = "NCMEDIA_USERNAME"
	NCMEDIA_PASSWORD         = "NCMEDIA_PASSWORD"
	NCMEDIA_DISCORD_WEBHOOK  = "NCMEDIA_DISCORD_WEBHOOK"
)

// main is the entry point of the application
func main() {
	// connect to minio
	minioClient, err := connectToMinio()
	if err != nil {
		log.Fatalf("Failed to connect to MinIO: %v", err)
	}

	// setup routes
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", heartbeatHandler)
	mux.Handle("GET /download/{bucketName}/{objectName}", chainMiddlewares(downloadHandler(minioClient), authMiddleware))
	mux.Handle("POST /upload/{bucketName}", chainMiddlewares(uploadHandler(minioClient), authMiddleware))

	// setup middleware
	handler := chainMiddlewares(mux, recoverMiddleware, timeoutMiddleware)
	// start server
	http.ListenAndServe(getEnv(NCMEDIA_ADDR, ":8083"), handler)
}

// getEnv returns the value of the environment variable key or defaultValue if key is not set
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// connectToMinio connects to MinIO server
func connectToMinio() (*minio.Client, error) {
	endpoint := getEnv(NCMEDIA_MINIO_ENDPOINT, "localhost:9000")
	accessKey := getEnv(NCMEDIA_MINIO_ACCESS_KEY, "root")
	secretKey := getEnv(NCMEDIA_MINIO_SECRET_KEY, "password")
	useSSL, _ := strconv.ParseBool(getEnv(NCMEDIA_MINIO_USE_SSL, "false"))
	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
}

// sendErrorToDiscord sends an error message to Discord webhook
func sendErrorToDiscord(message string, data any) {
	webhookUrl := getEnv(NCMEDIA_DISCORD_WEBHOOK, "")
	if webhookUrl == "" {
		return
	}

	tz, _ := time.LoadLocation("Asia/Jakarta")
	title := fmt.Sprintf("%s [%s] %s.%s", ":poop:", time.Now().In(tz).Format(time.RFC822), "log", "ERROR")
	jsonPretty, _ := json.MarshalIndent(data, "", "  ")
	rawBody, _ := json.Marshal(map[string]any{
		"username": "ncmedia",
		"embeds": []map[string]any{
			{
				"title":       title,
				"description": ":black_small_square: " + message,
				"color":       0xe67e22,
			},
			{
				"title":       "",
				"description": "**Context**\n`" + string(jsonPretty) + "`",
				"color":       0xe67e22,
			},
		},
	})
	req, err := http.NewRequest("POST", webhookUrl, bytes.NewBuffer(rawBody))
	if err != nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
}

// middleware is a function that wraps an http.Handler
type middleware func(http.Handler) http.Handler

// chainMiddlewares chains multiple middlewares together
func chainMiddlewares(h http.Handler, m ...middleware) http.Handler {
	if len(m) < 1 {
		return h
	}

	wrapped := h
	for i := len(m) - 1; i >= 0; i-- {
		wrapped = m[i](wrapped)
	}

	return wrapped
}

// recoverMiddleware recovers from panics and sends error to Discord
func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rvr := recover(); rvr != nil {
				if err, ok := rvr.(error); ok {
					if errors.Is(err, context.Canceled) || errors.Is(err, http.ErrAbortHandler) {
						return
					}

					if getEnv(NCMEDIA_ENV, "development") == "production" {
						go sendErrorToDiscord(err.Error(), map[string]any{
							"ip":         r.RemoteAddr,
							"host":       r.Host,
							"method":     r.Method,
							"uri":        r.RequestURI,
							"user_agent": r.UserAgent(),
						})
					} else {
						log.Println(rvr)
					}

					w.WriteHeader(http.StatusInternalServerError)
				}
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// timeoutMiddleware sets a timeout for the request
func timeoutMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timeoutStr := os.Getenv(NCMEDIA_REQUEST_TIMEOUT)
		timeout := 30 * time.Second
		if timeoutStr != "" {
			if t, err := time.ParseDuration(timeoutStr); err == nil {
				timeout = t
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer func() {
			cancel()
			if ctx.Err() == context.DeadlineExceeded {
				w.WriteHeader(http.StatusRequestTimeout)
			}
		}()

		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware authenticates the request using Basic Auth
func authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		envUsername := getEnv(NCMEDIA_USERNAME, "ncmedia")
		envPassword := getEnv(NCMEDIA_PASSWORD, "ncmedia")
		if username != envUsername || password != envPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// resJSON writes a JSON response with the given data and status code
func resJSON(w http.ResponseWriter, data any, status int) {
	// convert error to map
	if err, ok := data.(error); ok {
		data = map[string]any{"message": err.Error()}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		panic(err)
	}
}

// heartbeatHandler returns a simple "OK" response
func heartbeatHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

// uploadHandler handles file upload requests
func uploadHandler(minioClient *minio.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// get file from form
		file, fileHeader, err := r.FormFile("file")
		if err != nil {
			// check if file is missing
			if errors.Is(err, http.ErrMissingFile) {
				resJSON(w, errors.New("file is required"), http.StatusBadRequest)
				return
			}
			panic(err)
		}
		defer file.Close()

		bucketName := r.PathValue("bucketName")
		overwrite, _ := strconv.ParseBool(r.URL.Query().Get("overwrite"))
		// check if file already exists and overwrite is false
		if !overwrite {
			statObjectOptions := minio.StatObjectOptions{}
			// set query params to stat object options
			queryParams := r.URL.Query()
			for key, values := range queryParams {
				for _, value := range values {
					statObjectOptions.SetReqParam(key, value)
				}
			}
			// check if file exists
			_, err := minioClient.StatObject(r.Context(), bucketName, fileHeader.Filename, statObjectOptions)
			if err == nil {
				resJSON(w, map[string]any{"message": "file already exists and skip overwrite"}, http.StatusOK)
				return
			}

			errResponse := minio.ToErrorResponse(err)
			// check if bucket not found
			if errResponse.Code == minio.NoSuchBucket {
				resJSON(w, errors.New("media not found"), http.StatusNotFound)
				return
			}
			// check if file not found (expected error when file doesn't exist)
			if errResponse.Code != minio.NoSuchKey {
				panic(err)
			}
		}

		mimeType := r.FormValue("mime_type")
		// fallback to content type from header
		if mimeType == "" {
			mimeType = fileHeader.Header.Get("Content-Type")
		}

		putObjectOptions := minio.PutObjectOptions{
			ContentType: mimeType,
		}
		if tags := r.FormValue("tags"); tags != "" {
			putObjectOptions.UserTags = map[string]string{"owner": tags}
		}
		if _, err = minioClient.PutObject(r.Context(), bucketName, fileHeader.Filename, file, fileHeader.Size, putObjectOptions); err != nil {
			errResponse := minio.ToErrorResponse(err)
			// check if bucket not found
			if errResponse.Code == minio.NoSuchBucket {
				resJSON(w, errors.New("media not found"), http.StatusNotFound)
				return
			}
			panic(err)
		}

		resJSON(w, map[string]any{"message": "file uploaded successfully"}, http.StatusOK)
	}
}

// downloadHandler handles file download requests
func downloadHandler(minioClient *minio.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucketName := r.PathValue("bucketName")
		objectName := r.PathValue("objectName")
		objectOptions := minio.GetObjectOptions{}
		// set request parameters
		queryParams := r.URL.Query()
		for key, values := range queryParams {
			for _, value := range values {
				objectOptions.SetReqParam(key, value)
			}
		}
		// get object from minio
		object, err := minioClient.GetObject(r.Context(), bucketName, objectName, objectOptions)
		if err != nil {
			panic(err)
		}
		defer object.Close()

		// get object info
		objectInfo, err := object.Stat()
		if err != nil {
			errResponse := minio.ToErrorResponse(err)
			// check if object or bucket not found
			if errResponse.Code == minio.NoSuchKey || errResponse.Code == minio.NoSuchBucket {
				resJSON(w, errors.New("media not found"), http.StatusNotFound)
				return
			}
			panic(err)
		}

		// set response headers
		w.Header().Set("Content-Type", objectInfo.ContentType)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", objectInfo.Size))
		// copy object to response
		if _, err := io.Copy(w, object); err != nil {
			panic(err)
		}
	}
}
