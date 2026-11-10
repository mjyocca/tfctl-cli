// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/go-tfe/v2/api/models"
)

const defaultConfigurationVersionPollInterval = 2 * time.Second

// ConfigurationVersionCreateOptions controls a new configuration version.
type ConfigurationVersionCreateOptions struct {
	AutoQueueRuns bool
	Speculative   bool
	Provisional   bool
}

// CreatedConfigurationVersion contains the result needed to upload an archive.
// The bearer capability remains private to this package.
type CreatedConfigurationVersion struct {
	ID        string
	Status    string
	uploadURL string
}

func (v CreatedConfigurationVersion) String() string {
	return fmt.Sprintf("configuration version %s (%s)", v.ID, v.Status)
}

func (v CreatedConfigurationVersion) GoString() string {
	return v.String()
}

// ConfigurationVersionStatus is the safe status of a configuration version.
type ConfigurationVersionStatus struct {
	ID     string
	Status string
}

// CreateConfigurationVersion creates an upload target for workspaceID.
func (c *Client) CreateConfigurationVersion(ctx context.Context, workspaceID string, opts ConfigurationVersionCreateOptions) (CreatedConfigurationVersion, error) {
	attrs := models.NewConfigurationVersions_attributes()
	attrs.SetAutoQueueRuns(&opts.AutoQueueRuns)
	attrs.SetSpeculative(&opts.Speculative)
	attrs.SetProvisional(&opts.Provisional)

	typeValue := models.CONFIGURATIONVERSIONS_CONFIGURATIONVERSIONS_TYPE
	data := models.NewConfigurationVersions()
	data.SetTypeEscaped(&typeValue)
	data.SetAttributes(attrs)
	envelope := models.NewConfigurationVersionsEnvelope()
	envelope.SetData(data)

	response, err := c.TFE.API.Workspaces().ByWorkspace_id(workspaceID).ConfigurationVersions().Post(ctx, envelope, nil)
	if err != nil {
		return CreatedConfigurationVersion{}, fmt.Errorf("create configuration version: %w", err)
	}
	if response == nil || response.GetData() == nil || response.GetData().GetId() == nil {
		return CreatedConfigurationVersion{}, fmt.Errorf("create configuration version: response has no ID")
	}
	id := *response.GetData().GetId()
	responseAttrs := response.GetData().GetAttributes()
	if responseAttrs == nil || responseAttrs.GetUploadUrl() == nil || *responseAttrs.GetUploadUrl() == "" {
		return CreatedConfigurationVersion{}, fmt.Errorf("configuration version %s was created, but the server did not provide an archive upload capability", id)
	}
	status := "pending"
	if responseAttrs.GetStatus() != nil {
		status = responseAttrs.GetStatus().String()
	}
	return CreatedConfigurationVersion{ID: id, Status: status, uploadURL: *responseAttrs.GetUploadUrl()}, nil
}

// ConfigurationVersionStatus reads the current safe status for id.
func (c *Client) ConfigurationVersionStatus(ctx context.Context, id string) (ConfigurationVersionStatus, error) {
	response, err := c.TFE.API.ConfigurationVersions().ByConfiguration_version_id(id).Get(ctx, nil)
	if err != nil {
		return ConfigurationVersionStatus{}, fmt.Errorf("read configuration version %s: %w", id, err)
	}
	if response == nil || response.GetData() == nil || response.GetData().GetAttributes() == nil || response.GetData().GetAttributes().GetStatus() == nil {
		return ConfigurationVersionStatus{}, fmt.Errorf("configuration version %s has no status", id)
	}
	return ConfigurationVersionStatus{ID: id, Status: response.GetData().GetAttributes().GetStatus().String()}, nil
}

// PollConfigurationVersionUntilUploaded waits until id is uploaded or unusable.
func (c *Client) PollConfigurationVersionUntilUploaded(ctx context.Context, id string, interval time.Duration, statusUpdate func(string)) (ConfigurationVersionStatus, error) {
	if interval <= 0 {
		interval = defaultConfigurationVersionPollInterval
	}
	last := ""
	for {
		status, err := c.ConfigurationVersionStatus(ctx, id)
		if err != nil {
			return ConfigurationVersionStatus{}, err
		}
		if status.Status != last {
			if statusUpdate != nil {
				statusUpdate(status.Status)
			}
			last = status.Status
		}

		switch status.Status {
		case "uploaded":
			return status, nil
		case "errored", "archived", "backing_data_soft_deleted", "backing_data_permanently_deleted":
			return status, fmt.Errorf("configuration version %s reached unusable status %q", id, status.Status)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, ctx.Err()
		case <-timer.C:
		}
	}
}
