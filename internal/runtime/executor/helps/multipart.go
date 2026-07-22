package helps

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	log "github.com/sirupsen/logrus"
)

const (
	DefaultMultipartBodyBytes int64 = 128 << 20
	DefaultMultipartFileBytes int64 = 20 << 20
)

// ValidateMultipartPayloadSize protects direct SDK executor calls that bypass
// HTTP request-body limits.
func ValidateMultipartPayloadSize(payload []byte, maxBytes int64) error {
	if maxBytes > 0 && int64(len(payload)) > maxBytes {
		return multipartRequestTooLarge(fmt.Sprintf("multipart request exceeds %d bytes", maxBytes))
	}
	return nil
}

// ValidateMultipartFormFiles applies the same per-file budget to every field,
// including fields an endpoint does not otherwise consume.
func ValidateMultipartFormFiles(form *multipart.Form, maxBytes int64) error {
	if form == nil || maxBytes <= 0 {
		return nil
	}
	for _, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader != nil && fileHeader.Size > maxBytes {
				return multipartRequestTooLarge(fmt.Sprintf("upload file %q exceeds %d bytes", fileHeader.Filename, maxBytes))
			}
		}
	}
	return nil
}

// ReadMultipartFile reads one uploaded file with a limit+1 overflow check.
func ReadMultipartFile(fileHeader *multipart.FileHeader, maxBytes int64) ([]byte, error) {
	if fileHeader == nil {
		return nil, fmt.Errorf("upload file is nil")
	}
	if maxBytes > 0 && fileHeader.Size > maxBytes {
		return nil, multipartRequestTooLarge(fmt.Sprintf("upload file %q exceeds %d bytes", fileHeader.Filename, maxBytes))
	}
	var body bytes.Buffer
	if maxBytes > 0 {
		body.Grow(int(min(fileHeader.Size, maxBytes)))
	}
	if err := CopyMultipartFile(&body, fileHeader, maxBytes); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// CopyMultipartFile copies one uploaded file with a limit+1 overflow check.
func CopyMultipartFile(dst io.Writer, fileHeader *multipart.FileHeader, maxBytes int64) error {
	if dst == nil {
		return fmt.Errorf("multipart destination is nil")
	}
	if fileHeader == nil {
		return fmt.Errorf("upload file is nil")
	}
	if maxBytes > 0 && fileHeader.Size > maxBytes {
		return multipartRequestTooLarge(fmt.Sprintf("upload file %q exceeds %d bytes", fileHeader.Filename, maxBytes))
	}
	src, err := fileHeader.Open()
	if err != nil {
		return fmt.Errorf("open upload file failed: %w", err)
	}
	defer func() {
		if errClose := src.Close(); errClose != nil {
			log.Errorf("executor multipart: close upload file: %v", errClose)
		}
	}()

	reader := io.Reader(src)
	if maxBytes > 0 {
		reader = io.LimitReader(src, maxBytes+1)
	}
	written, err := io.Copy(dst, reader)
	if err != nil {
		return fmt.Errorf("copy upload file failed: %w", err)
	}
	if maxBytes > 0 && written > maxBytes {
		return multipartRequestTooLarge(fmt.Sprintf("upload file %q exceeds %d bytes", fileHeader.Filename, maxBytes))
	}
	return nil
}

func multipartRequestTooLarge(message string) error {
	cause := fmt.Errorf("%s", message)
	return &failurecontract.Failure{
		Kind:          failurecontract.RequestTooLarge,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    http.StatusRequestEntityTooLarge,
		ProviderCode:  "request_too_large",
		Cause:         cause,
		PublicMessage: message,
	}
}
