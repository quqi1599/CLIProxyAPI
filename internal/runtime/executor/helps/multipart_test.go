package helps

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

func TestMultipartLimitsUseCanonicalRequestFailure(t *testing.T) {
	assertTooLarge := func(t *testing.T, err error) {
		t.Helper()
		typed, ok := failurecontract.As(err)
		if !ok || typed.Kind != failurecontract.RequestTooLarge || typed.Scope != failurecontract.ScopeRequest || typed.HTTPStatus != http.StatusRequestEntityTooLarge || typed.ProviderCode != "request_too_large" {
			t.Fatalf("failure = %#v", typed)
		}
	}

	assertTooLarge(t, ValidateMultipartPayloadSize([]byte("12345"), 4))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err = part.Write([]byte("12345")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, err := reader.ReadForm(1)
	if err != nil {
		t.Fatalf("read form: %v", err)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove form files: %v", errRemove)
		}
	}()

	assertTooLarge(t, ValidateMultipartFormFiles(form, 4))
	_, err = ReadMultipartFile(form.File["image"][0], 4)
	assertTooLarge(t, err)
}

func TestReadMultipartFileAcceptsExactLimit(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err = part.Write([]byte("1234")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, err := reader.ReadForm(1)
	if err != nil {
		t.Fatalf("read form: %v", err)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove form files: %v", errRemove)
		}
	}()

	data, err := ReadMultipartFile(form.File["image"][0], 4)
	if err != nil {
		t.Fatalf("ReadMultipartFile() error = %v", err)
	}
	if string(data) != "1234" {
		t.Fatalf("data = %q", data)
	}
}
