// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigurationArchiveUploader(t *testing.T) {
	t.Parallel()

	payload := []byte("configuration archive")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		assert.Equal(t, int64(len(payload)), r.ContentLength)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("X-Tfctl-Profile"))
		got, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, got)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	uploader := newTestConfigurationArchiveUploader(server.Client())
	err := uploader.Upload(context.Background(), server.URL+"/upload?token=CAPABILITY_SECRET", bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)
}

func TestConfigurationArchiveUploaderRejectsUnsafeURL(t *testing.T) {
	t.Parallel()

	uploader := NewConfigurationArchiveUploader()
	for _, rawURL := range []string{"", "/relative", "http://example.test/upload?token=SECRET"} {
		err := uploader.Upload(context.Background(), rawURL, bytes.NewReader(nil), 0)
		require.Error(t, err)
		if rawURL != "" {
			assert.NotContains(t, err.Error(), rawURL)
		}
		assert.NotContains(t, err.Error(), "SECRET")
	}
}

func TestConfigurationArchiveUploaderRejectsRedirect(t *testing.T) {
	t.Parallel()

	redirected := atomic.Bool{}
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(destination.Close)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	uploader := newTestConfigurationArchiveUploader(server.Client())
	err := uploader.Upload(context.Background(), server.URL+"?token=REDIRECT_SECRET", bytes.NewReader([]byte("body")), 4)
	require.ErrorContains(t, err, "redirect")
	assert.NotContains(t, err.Error(), "REDIRECT_SECRET")
	assert.False(t, redirected.Load())
}

func TestConfigurationArchiveUploaderRetriesReplayableFailure(t *testing.T) {
	t.Parallel()

	payload := []byte("replayed exactly")
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		got, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, got)
		if attempt < 3 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	uploader := newTestConfigurationArchiveUploader(server.Client())
	err := uploader.Upload(context.Background(), server.URL, bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)
	assert.Equal(t, int32(3), attempts.Load())
}

func TestConfigurationArchiveUploaderDoesNotRetryPermanentFailure(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "signed token expired", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	secretURL := server.URL + "/upload?token=CAPABILITY_SECRET"
	uploader := newTestConfigurationArchiveUploader(server.Client())
	err := uploader.Upload(context.Background(), secretURL, bytes.NewReader([]byte("body")), 4)
	require.ErrorContains(t, err, "403 Forbidden")
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, err.Error(), "CAPABILITY_SECRET")
	assert.NotContains(t, err.Error(), "signed token expired")
	assert.Equal(t, int32(1), attempts.Load())
}

func TestConfigurationArchiveUploaderSanitizesTransportError(t *testing.T) {
	t.Parallel()

	secretURL := "https://upload.example.test/archive?token=CAPABILITY_SECRET"
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failure at " + secretURL)
	})}
	uploader := newTestConfigurationArchiveUploader(httpClient)
	uploader.maxAttempts = 1

	err := uploader.Upload(context.Background(), secretURL, bytes.NewReader(nil), 0)
	require.ErrorContains(t, err, "network request failed")
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, err.Error(), "CAPABILITY_SECRET")
}

func TestConfigurationArchiveUploaderHonorsCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	uploader := newTestConfigurationArchiveUploader(server.Client())
	done := make(chan error, 1)
	go func() {
		done <- uploader.Upload(ctx, server.URL, strings.NewReader("body"), 4)
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("upload did not stop after cancellation")
	}
}

func newTestConfigurationArchiveUploader(httpClient *http.Client) *ConfigurationArchiveUploader {
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return errConfigurationUploadRedirect }
	return &ConfigurationArchiveUploader{
		httpClient:  &clientCopy,
		maxAttempts: 3,
		retryDelay: func(context.Context, int) error {
			return nil
		},
	}
}
