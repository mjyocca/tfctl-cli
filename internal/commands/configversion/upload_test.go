// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package configversion

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/tfctl-cli/internal/commands/cmdtest"
	"github.com/hashicorp/tfctl-cli/internal/pkg/client"
	"github.com/hashicorp/tfctl-cli/internal/pkg/configurationarchive"
	"github.com/hashicorp/tfctl-cli/internal/pkg/format"
	"github.com/hashicorp/tfctl-cli/internal/pkg/iostreams"
)

func TestRunUpload(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		source := configurationSource(t)
		uploader := &testUploader{}
		var statusGets atomic.Int32
		server := cmdtest.NewServer(t, cmdtest.RouteMap{
			"POST /api/v2/workspaces/ws-abc123/configuration-versions": func(w http.ResponseWriter, _ *http.Request) {
				cmdtest.WriteJSONAPI(w, configurationVersionJSON("cv-new123", "pending", "https://upload.example.test/archive?token=CAPABILITY_SECRET"))
			},
			"GET /api/v2/configuration-versions/cv-new123": func(w http.ResponseWriter, _ *http.Request) {
				status := "pending"
				if statusGets.Add(1) > 1 {
					status = "uploaded"
				}
				cmdtest.WriteJSONAPI(w, configurationVersionJSON("cv-new123", status, ""))
			},
		})

		err := runUpload(context.Background(), UploadOpts{
			IO:           ioStreams,
			Output:       format.New(ioStreams),
			APIClient:    commandTestClient(t, server.URL),
			Uploader:     uploader,
			Pack:         configurationarchive.Pack,
			Workspace:    "ws-abc123",
			Directory:    source,
			PollInterval: time.Millisecond,
		})
		require.NoError(t, err)
		assert.True(t, uploader.called)
		assert.Equal(t, "cv-new123", uploader.targetID)
		assert.Positive(t, uploader.size)
		assert.Contains(t, ioStreams.Output.String(), "cv-new123")
		assert.Contains(t, ioStreams.Output.String(), "uploaded")
		assert.NotContains(t, ioStreams.Output.String(), "CAPABILITY_SECRET")
		assert.NotContains(t, ioStreams.Error.String(), "CAPABILITY_SECRET")
	})

	t.Run("workspace name resolves in organization", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		server := cmdtest.NewServer(t, cmdtest.RouteMap{
			"GET /api/v2/organizations/my-org/workspaces/my-workspace": func(w http.ResponseWriter, _ *http.Request) {
				cmdtest.WriteJSONAPI(w, map[string]any{
					"data": map[string]any{"id": "ws-resolved", "type": "workspaces"},
				})
			},
		})

		err := runUpload(context.Background(), UploadOpts{
			IO:           ioStreams,
			Output:       format.New(ioStreams),
			APIClient:    commandTestClient(t, server.URL),
			Uploader:     &testUploader{},
			Pack:         configurationarchive.Pack,
			Workspace:    "my-workspace",
			Organization: "my-org",
			Directory:    configurationSource(t),
			DryRun:       true,
		})
		require.NoError(t, err)
		assert.Contains(t, ioStreams.Error.String(), "workspace ws-resolved")
	})

	t.Run("dry run does not package or mutate", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		source := configurationSource(t)
		var packed atomic.Bool
		server := cmdtest.NewServer(t, cmdtest.RouteMap{})

		err := runUpload(context.Background(), UploadOpts{
			IO:        ioStreams,
			Output:    format.New(ioStreams),
			APIClient: commandTestClient(t, server.URL),
			Uploader:  &testUploader{},
			Pack: func(string) (*configurationarchive.Archive, error) {
				packed.Store(true)
				return nil, errors.New("must not run")
			},
			Workspace: "ws-abc123",
			Directory: source,
			DryRun:    true,
		})
		require.NoError(t, err)
		assert.False(t, packed.Load())
		assert.Contains(t, ioStreams.Error.String(), "would package")
	})

	t.Run("pack failure occurs before create", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		server := cmdtest.NewServer(t, cmdtest.RouteMap{})
		err := runUpload(context.Background(), UploadOpts{
			IO:        ioStreams,
			Output:    format.New(ioStreams),
			APIClient: commandTestClient(t, server.URL),
			Uploader:  &testUploader{},
			Pack: func(string) (*configurationarchive.Archive, error) {
				return nil, errors.New("pack failed")
			},
			Workspace: "ws-abc123",
			Directory: configurationSource(t),
		})
		require.ErrorContains(t, err, "pack failed")
	})

	t.Run("upload failure reports ID without capability", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		server := cmdtest.NewServer(t, cmdtest.RouteMap{
			"POST /api/v2/workspaces/ws-abc123/configuration-versions": func(w http.ResponseWriter, _ *http.Request) {
				cmdtest.WriteJSONAPI(w, configurationVersionJSON("cv-new123", "pending", "https://upload.example.test/archive?token=CAPABILITY_SECRET"))
			},
		})

		err := runUpload(context.Background(), UploadOpts{
			IO:        ioStreams,
			Output:    format.New(ioStreams),
			APIClient: commandTestClient(t, server.URL),
			Uploader:  &testUploader{err: errors.New("upload failed")},
			Pack:      configurationarchive.Pack,
			Workspace: "ws-abc123",
			Directory: configurationSource(t),
		})
		require.ErrorContains(t, err, "cv-new123")
		assert.NotContains(t, err.Error(), "CAPABILITY_SECRET")
	})

	t.Run("upload cancellation preserves identity and cleanup failure", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		server := cmdtest.NewServer(t, cmdtest.RouteMap{
			"POST /api/v2/workspaces/ws-abc123/configuration-versions": func(w http.ResponseWriter, _ *http.Request) {
				cmdtest.WriteJSONAPI(w, configurationVersionJSON("cv-new123", "pending", "https://upload.example.test/archive?token=CAPABILITY_SECRET"))
			},
		})

		err := runUpload(context.Background(), UploadOpts{
			IO:        ioStreams,
			Output:    format.New(ioStreams),
			APIClient: commandTestClient(t, server.URL),
			Uploader:  &testUploader{err: context.Canceled, skipRead: true},
			Pack: func(source string) (*configurationarchive.Archive, error) {
				archive, err := configurationarchive.Pack(source)
				if err != nil {
					return nil, err
				}
				file, ok := archive.Reader().(*os.File)
				require.True(t, ok)
				require.NoError(t, file.Close())
				return archive, nil
			},
			Workspace: "ws-abc123",
			Directory: configurationSource(t),
		})
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "cv-new123")
		require.ErrorContains(t, err, "close temporary configuration archive")
		assert.NotContains(t, err.Error(), "CAPABILITY_SECRET")
	})

	t.Run("no wait skips polling", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		server := cmdtest.NewServer(t, cmdtest.RouteMap{
			"POST /api/v2/workspaces/ws-abc123/configuration-versions": func(w http.ResponseWriter, _ *http.Request) {
				cmdtest.WriteJSONAPI(w, configurationVersionJSON("cv-new123", "pending", "https://upload.example.test/archive"))
			},
		})
		err := runUpload(context.Background(), UploadOpts{
			IO:        ioStreams,
			Output:    format.New(ioStreams),
			APIClient: commandTestClient(t, server.URL),
			Uploader:  &testUploader{},
			Pack:      configurationarchive.Pack,
			Workspace: "ws-abc123",
			Directory: configurationSource(t),
			NoWait:    true,
		})
		require.NoError(t, err)
		assert.Contains(t, ioStreams.Output.String(), "pending")
	})
}

type testUploader struct {
	called   bool
	targetID string
	size     int64
	err      error
	skipRead bool
}

func (u *testUploader) Upload(_ context.Context, target client.CreatedConfigurationVersion, archive io.ReadSeeker, size int64) error {
	u.called = true
	u.targetID = target.ID
	u.size = size
	if !u.skipRead {
		_, err := io.Copy(io.Discard, archive)
		if err != nil {
			return err
		}
	}
	return u.err
}

func configurationSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte("terraform {}\n"), 0o600))
	return dir
}

func commandTestClient(t *testing.T, address string) *client.Client {
	t.Helper()
	c, err := client.New(context.Background(), address, "API_SECRET", nil)
	require.NoError(t, err)
	return c
}

func configurationVersionJSON(id, status, uploadURL string) map[string]any {
	attrs := map[string]any{"status": status}
	if uploadURL != "" {
		attrs["upload-url"] = uploadURL
	}
	return map[string]any{
		"data": map[string]any{
			"id": id, "type": "configuration-versions", "attributes": attrs,
		},
	}
}

func TestUploadDisplayerOutput(t *testing.T) {
	t.Parallel()
	result := UploadResult{
		ID:               "cv-abc123",
		WorkspaceID:      "ws-abc123",
		Status:           "uploaded",
		Files:            2,
		UncompressedSize: 10,
		CompressedSize:   8,
	}

	t.Run("JSON", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		output := format.New(ioStreams)
		output.SetFormat(format.JSON)
		require.NoError(t, output.Display(uploadDisplayer{result: result}))
		assert.Contains(t, ioStreams.Output.String(), `"id": "cv-abc123"`)
		assert.Contains(t, ioStreams.Output.String(), `"workspace_id": "ws-abc123"`)
		assert.NotContains(t, ioStreams.Output.String(), "upload-url")
	})

	t.Run("jq ID", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		output := format.New(ioStreams)
		output.SetFormat(format.JSON)
		output.SetJQFilter(".id")
		require.NoError(t, output.Display(uploadDisplayer{result: result}))
		assert.Equal(t, "cv-abc123\n", ioStreams.Output.String())
	})

	t.Run("Markdown", func(t *testing.T) {
		t.Parallel()
		ioStreams := iostreams.Test()
		output := format.New(ioStreams)
		output.SetFormat(format.Markdown)
		require.NoError(t, output.Display(uploadDisplayer{result: result}))
		assert.Contains(t, ioStreams.Output.String(), "cv-abc123")
		assert.Contains(t, ioStreams.Output.String(), "uploaded")
	})
}
