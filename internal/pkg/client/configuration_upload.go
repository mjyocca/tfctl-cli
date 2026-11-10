// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	configurationUploadAttempts = 3
	configurationUploadTimeout  = 10 * time.Minute
	maxUploadResponseBody       = 8 << 10
)

var errConfigurationUploadRedirect = errors.New("configuration upload redirect refused")

// ConfigurationArchiveUploader sends an archive to a server-provided capability URL.
// It uses a separate HTTP client and never adds HCP Terraform API credentials.
type ConfigurationArchiveUploader struct {
	httpClient  *http.Client
	maxAttempts int
	retryDelay  func(context.Context, int) error
}

// NewConfigurationArchiveUploader creates an uploader with secure transport defaults.
func NewConfigurationArchiveUploader() *ConfigurationArchiveUploader {
	return &ConfigurationArchiveUploader{
		httpClient: &http.Client{
			Transport: newConfigurationUploadTransport(),
			Timeout:   configurationUploadTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errConfigurationUploadRedirect
			},
		},
		maxAttempts: configurationUploadAttempts,
		retryDelay: func(ctx context.Context, attempt int) error {
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func newConfigurationUploadTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Upload sends archive to capabilityURL. The archive is rewound before each attempt.
func (u *ConfigurationArchiveUploader) Upload(ctx context.Context, capabilityURL string, archive io.ReadSeeker, size int64) error {
	parsed, err := url.Parse(capabilityURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("configuration archive upload URL is invalid or insecure")
	}
	if archive == nil {
		return fmt.Errorf("configuration archive is not readable")
	}
	if size < 0 {
		return fmt.Errorf("configuration archive size is invalid")
	}

	attempts := u.maxAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind configuration archive: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, parsed.String(), io.NopCloser(archive))
		if err != nil {
			return fmt.Errorf("create configuration archive upload request")
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
		req.ContentLength = size

		resp, requestErr := u.httpClient.Do(req)
		if requestErr != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("configuration archive upload canceled: %w", ctx.Err())
			}
			if errors.Is(requestErr, errConfigurationUploadRedirect) {
				return fmt.Errorf("configuration archive upload failed: redirect refused")
			}
			if attempt == attempts {
				return fmt.Errorf("configuration archive upload failed: network request failed")
			}
			if err := u.waitBeforeRetry(ctx, attempt); err != nil {
				return fmt.Errorf("configuration archive upload canceled: %w", err)
			}
			continue
		}

		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUploadResponseBody))
		closeErr := resp.Body.Close()
		if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
			return fmt.Errorf("configuration archive upload failed: redirect refused")
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			if closeErr != nil {
				return fmt.Errorf("configuration archive upload failed while closing response")
			}
			return nil
		}

		if !retryableUploadStatus(resp.StatusCode) || attempt == attempts {
			return fmt.Errorf("configuration archive upload failed: server returned %s", resp.Status)
		}
		if err := u.waitBeforeRetry(ctx, attempt); err != nil {
			return fmt.Errorf("configuration archive upload canceled: %w", err)
		}
	}
	return fmt.Errorf("configuration archive upload failed")
}

func (u *ConfigurationArchiveUploader) waitBeforeRetry(ctx context.Context, attempt int) error {
	if u.retryDelay == nil {
		return nil
	}
	return u.retryDelay(ctx, attempt)
}

func retryableUploadStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
