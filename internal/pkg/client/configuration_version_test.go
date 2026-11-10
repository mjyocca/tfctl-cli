// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateConfigurationVersion(t *testing.T) {
	t.Parallel()

	c := newTestClient(t)
	c.Adapter.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "/api/v2/workspaces/ws-abc123/configuration-versions", req.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		data := body["data"].(map[string]any)
		attrs := data["attributes"].(map[string]any)
		assert.Equal(t, false, attrs["auto-queue-runs"])
		assert.Equal(t, true, attrs["speculative"])
		assert.Equal(t, true, attrs["provisional"])

		return configurationVersionResponse(req, `{
          "data": {
            "id": "cv-new123",
            "type": "configuration-versions",
            "attributes": {
              "status": "pending",
              "upload-url": "https://upload.example.test/archive?token=CAPABILITY_SECRET"
            }
          }
        }`), nil
	})

	created, err := c.CreateConfigurationVersion(context.Background(), "ws-abc123", ConfigurationVersionCreateOptions{
		Speculative: true,
		Provisional: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "cv-new123", created.ID)
	assert.Equal(t, "https://upload.example.test/archive?token=CAPABILITY_SECRET", created.UploadURL)
}

func TestCreateConfigurationVersionRequiresUploadURL(t *testing.T) {
	t.Parallel()

	c := newTestClient(t)
	c.Adapter.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return configurationVersionResponse(req, `{"data":{"id":"cv-new123","type":"configuration-versions","attributes":{"status":"pending"}}}`), nil
	})

	_, err := c.CreateConfigurationVersion(context.Background(), "ws-abc123", ConfigurationVersionCreateOptions{})
	require.ErrorContains(t, err, "cv-new123")
	assert.NotContains(t, err.Error(), "upload-url")
}

func TestPollConfigurationVersionUntilUploaded(t *testing.T) {
	t.Parallel()

	var gets atomic.Int32
	c := newTestClient(t)
	c.Adapter.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := "pending"
		if gets.Add(1) > 1 {
			status = "uploaded"
		}
		return configurationVersionResponse(req, `{"data":{"id":"cv-abc123","type":"configuration-versions","attributes":{"status":"`+status+`"}}}`), nil
	})

	var statuses []string
	status, err := c.PollConfigurationVersionUntilUploaded(context.Background(), "cv-abc123", time.Millisecond, func(value string) {
		statuses = append(statuses, value)
	})
	require.NoError(t, err)
	assert.Equal(t, "uploaded", status.Status)
	assert.Equal(t, []string{"pending", "uploaded"}, statuses)
}

func TestPollConfigurationVersionUntilUploadedErrored(t *testing.T) {
	t.Parallel()

	c := newTestClient(t)
	c.Adapter.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return configurationVersionResponse(req, `{"data":{"id":"cv-abc123","type":"configuration-versions","attributes":{"status":"errored","error":"bad archive","error-message":"CAPABILITY_SECRET"}}}`), nil
	})

	_, err := c.PollConfigurationVersionUntilUploaded(context.Background(), "cv-abc123", time.Millisecond, nil)
	require.ErrorContains(t, err, "cv-abc123")
	assert.NotContains(t, err.Error(), "CAPABILITY_SECRET")
}

func TestPollConfigurationVersionUntilUploadedCancellation(t *testing.T) {
	t.Parallel()

	c := newTestClient(t)
	c.Adapter.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return configurationVersionResponse(req, `{"data":{"id":"cv-abc123","type":"configuration-versions","attributes":{"status":"pending"}}}`), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.PollConfigurationVersionUntilUploaded(ctx, "cv-abc123", time.Second, nil)
	require.Error(t, err)
}

func configurationVersionResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{"application/vnd.api+json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
