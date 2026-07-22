package misc

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
	log "github.com/sirupsen/logrus"
)

const (
	configTemplateFallbackURL = "https://raw.githubusercontent.com/caidaoli/CLIProxyAPI/refs/heads/main/config.example.yaml"
	maxConfigTemplateBytes    = 1 << 20
)

func CopyConfigTemplate(src, dst string) error {
	in, err := openConfigTemplate(src)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := in.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close source config file")
		}
	}()
	data, errRead := httpfetch.ReadBytes(in, maxConfigTemplateBytes)
	if errRead != nil {
		return fmt.Errorf("read config template: %w", errRead)
	}

	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := out.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close destination config file")
		}
	}()

	if _, err = out.Write(data); err != nil {
		return err
	}
	return out.Sync()
}

func openConfigTemplate(src string) (io.ReadCloser, error) {
	in, err := os.Open(src)
	if err == nil {
		return in, nil
	}
	if !errors.Is(err, os.ErrNotExist) || filepath.Base(src) != "config.example.yaml" {
		return nil, err
	}

	resp, errGet := http.Get(configTemplateFallbackURL)
	if errGet != nil {
		return nil, fmt.Errorf("download fallback config template: %w", errGet)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Warn("failed to close fallback config template response")
			}
		}()
		return nil, fmt.Errorf("download fallback config template: unexpected status %s", resp.Status)
	}
	return resp.Body, nil
}
