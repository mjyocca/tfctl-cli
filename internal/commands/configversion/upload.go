// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package configversion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/posener/complete"

	"github.com/hashicorp/tfctl-cli/internal/commands/cmdutil"
	"github.com/hashicorp/tfctl-cli/internal/pkg/client"
	"github.com/hashicorp/tfctl-cli/internal/pkg/cmd"
	"github.com/hashicorp/tfctl-cli/internal/pkg/configurationarchive"
	"github.com/hashicorp/tfctl-cli/internal/pkg/flagvalue"
	"github.com/hashicorp/tfctl-cli/internal/pkg/format"
	"github.com/hashicorp/tfctl-cli/internal/pkg/heredoc"
	"github.com/hashicorp/tfctl-cli/internal/pkg/iostreams"
	"github.com/hashicorp/tfctl-cli/version"
)

const defaultUploadWaitTimeout = 2 * time.Minute

type configurationArchiveUploader interface {
	Upload(context.Context, string, io.ReadSeeker, int64) error
}

// UploadResult is the safe, structured output from a configuration upload.
type UploadResult struct {
	ID               string `json:"id"`
	WorkspaceID      string `json:"workspace_id"`
	Status           string `json:"status"`
	Speculative      bool   `json:"speculative"`
	Provisional      bool   `json:"provisional"`
	AutoQueueRun     bool   `json:"auto_queue_run"`
	Files            int    `json:"files"`
	UncompressedSize int64  `json:"uncompressed_size"`
	CompressedSize   int64  `json:"compressed_size"`
}

// UploadOpts contains the dependencies and values for config-version upload.
type UploadOpts struct {
	IO           iostreams.IOStreams
	Output       *format.Outputter
	APIClient    *client.Client
	Uploader     configurationArchiveUploader
	Pack         func(string) (*configurationarchive.Archive, error)
	Workspace    string
	Organization string
	Directory    string
	Speculative  bool
	Provisional  bool
	AutoQueueRun bool
	NoWait       bool
	Timeout      time.Duration
	PollInterval time.Duration
	DryRun       bool
}

// NewCmdUpload creates the `config-version upload` command.
func NewCmdUpload(inv *cmd.Invocation) *cmd.Command {
	opts := UploadOpts{
		IO:        inv.IO,
		Output:    inv.Output,
		Directory: ".",
		Timeout:   defaultUploadWaitTimeout,
		Pack:      configurationarchive.Pack,
		Uploader:  client.NewConfigurationArchiveUploader(),
	}

	c := &cmd.Command{
		Name:      "upload",
		ShortHelp: "Package and upload a configuration for a workspace.",
		LongHelp: heredoc.New(inv.IO, heredoc.WithPreserveNewlines()).Mustf(`
		The {{ template "mdCodeOrBold" "%s config-version upload" }} command packages a
		Terraform configuration directory and uploads it to a workspace.

		The command disables automatic run creation by default. Use
		{{ template "mdCodeOrBold" "--auto-queue-run" }} to let HCP Terraform create a run.
		By default, the command waits until the configuration version is uploaded.

		WORKSPACE can be a workspace ID ({{ template "mdCodeOrBold" "ws-abc123" }}) or a workspace
		name. A workspace name requires an organization from the flag, profile, or local Terraform context.
		`, version.Name),
		Args: cmd.PositionalArguments{Args: []cmd.PositionalArgument{{
			Name:          "WORKSPACE",
			Documentation: "workspace ID, or workspace name",
		}}},
		Flags: cmd.Flags{Local: []*cmd.Flag{
			{
				Name:         "directory",
				Description:  "Terraform configuration directory to package",
				Value:        flagvalue.Simple(".", &opts.Directory),
				Autocomplete: complete.PredictDirs("*"),
			},
			{
				Name:        "organization",
				Description: "Organization name (defaults to profile or Terraform cloud context)",
				Value:       flagvalue.Simple("", &opts.Organization),
			},
			{
				Name:          "speculative",
				Description:   "Restrict the configuration version to speculative runs",
				Value:         flagvalue.Simple(false, &opts.Speculative),
				IsBooleanFlag: true,
			},
			{
				Name:          "provisional",
				Description:   "Do not immediately make this the workspace's current configuration",
				Value:         flagvalue.Simple(false, &opts.Provisional),
				IsBooleanFlag: true,
			},
			{
				Name:          "auto-queue-run",
				Description:   "Let HCP Terraform create a run after upload",
				Value:         flagvalue.Simple(false, &opts.AutoQueueRun),
				IsBooleanFlag: true,
			},
			{
				Name:          "no-wait",
				Description:   "Return after the archive upload without waiting for processing",
				Value:         flagvalue.Simple(false, &opts.NoWait),
				IsBooleanFlag: true,
			},
			{
				Name:        "timeout",
				Description: "Maximum time to wait for the configuration version to become usable",
				Value:       flagvalue.Duration(defaultUploadWaitTimeout, &opts.Timeout),
			},
		}},
		Examples: []cmd.Example{
			{
				Preamble: "Upload the current directory to a workspace",
				Command:  heredoc.New(inv.IO, heredoc.WithNoWrap()).Mustf(`$ %s config-version upload my-workspace --organization my-org`, version.Name),
			},
			{
				Preamble: "Upload a directory and return only its configuration-version ID",
				Command:  heredoc.New(inv.IO, heredoc.WithNoWrap()).Mustf(`$ %s config-version upload ws-abc123 --directory ./production --jq '.id'`, version.Name),
			},
		},
		RunF: func(_ *cmd.Command, args []string) error {
			if len(args) != 1 {
				return cmd.ErrDisplayUsage
			}
			opts.Workspace = args[0]
			opts.Organization = cmdutil.ResolveOrganization(inv.Profile.DefaultOrganization, opts.Organization)
			opts.DryRun = inv.IsDryRun()
			apiClient, err := inv.NewAPIClient()
			if err != nil {
				return fmt.Errorf("unable to create API client: %w", err)
			}
			opts.APIClient = apiClient
			return runUpload(inv.ShutdownCtx, opts)
		},
	}
	return c
}

func runUpload(ctx context.Context, opts UploadOpts) error {
	if opts.APIClient == nil || opts.Output == nil || opts.Pack == nil || opts.Uploader == nil {
		return errors.New("configuration upload is not initialized")
	}
	workspaceID, err := resolveUploadWorkspace(ctx, opts)
	if err != nil {
		return err
	}
	if err := validateUploadDirectory(opts.Directory); err != nil {
		return err
	}
	if opts.DryRun {
		cs := opts.IO.ColorScheme()
		fmt.Fprintf(opts.IO.Err(), "%s would package %q and upload it as a configuration version for workspace %s\n", cs.DryRunLabel(), opts.Directory, workspaceID)
		fmt.Fprintf(opts.IO.Err(), "%s auto-queue-runs=%t, speculative=%t, provisional=%t\n", cs.DryRunLabel(), opts.AutoQueueRun, opts.Speculative, opts.Provisional)
		return nil
	}

	fmt.Fprintf(opts.IO.ErrUnessential(), "Packaging configuration from %s...\n", opts.Directory)
	archive, err := opts.Pack(opts.Directory)
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close() }()

	created, err := opts.APIClient.CreateConfigurationVersion(ctx, workspaceID, client.ConfigurationVersionCreateOptions{
		AutoQueueRuns: opts.AutoQueueRun,
		Speculative:   opts.Speculative,
		Provisional:   opts.Provisional,
	})
	if err != nil {
		return err
	}

	reader, err := archive.Open()
	if err != nil {
		return fmt.Errorf("configuration version %s was created, but its archive could not be opened: %w", created.ID, err)
	}
	uploadErr := opts.Uploader.Upload(ctx, created.UploadURL, reader, archive.Metadata.CompressedSize)
	closeErr := reader.Close()
	if uploadErr != nil {
		return fmt.Errorf("configuration version %s was created, but its archive upload failed: %w", created.ID, sanitizeCapabilityError(uploadErr, created.UploadURL))
	}
	if closeErr != nil {
		return fmt.Errorf("configuration version %s was uploaded, but the local archive could not be closed: %w", created.ID, closeErr)
	}

	status := created.Status
	if !opts.NoWait {
		waitCtx := ctx
		if opts.Timeout > 0 {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
		}
		final, err := opts.APIClient.PollConfigurationVersionUntilUploaded(waitCtx, created.ID, opts.PollInterval, func(value string) {
			fmt.Fprintf(opts.IO.ErrUnessential(), "Configuration version %s: %s\n", created.ID, value)
		})
		if err != nil {
			return fmt.Errorf("configuration version %s was uploaded, but did not become usable: %w", created.ID, err)
		}
		status = final.Status
	}

	result := UploadResult{
		ID:               created.ID,
		WorkspaceID:      workspaceID,
		Status:           status,
		Speculative:      opts.Speculative,
		Provisional:      opts.Provisional,
		AutoQueueRun:     opts.AutoQueueRun,
		Files:            archive.Metadata.Files,
		UncompressedSize: archive.Metadata.UncompressedSize,
		CompressedSize:   archive.Metadata.CompressedSize,
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := opts.Output.Display(uploadDisplayer{result: result}); err != nil {
		return err
	}
	return nil
}

func resolveUploadWorkspace(ctx context.Context, opts UploadOpts) (string, error) {
	if strings.HasPrefix(opts.Workspace, "ws-") {
		return opts.Workspace, nil
	}
	if opts.Organization == "" {
		return "", errors.New("organization is required when WORKSPACE is a name; use --organization or set a profile default")
	}
	workspace, err := client.NewResolver(opts.APIClient, false, opts.DryRun).Workspace(ctx, opts.Organization, opts.Workspace)
	if err != nil {
		return "", err
	}
	if workspace.GetId() == nil {
		return "", fmt.Errorf("resolved workspace %q has no ID", opts.Workspace)
	}
	return *workspace.GetId(), nil
}

func validateUploadDirectory(directory string) error {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve configuration directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("inspect configuration directory %q: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("configuration directory must not be a symbolic link")
	}
	if !info.IsDir() {
		return fmt.Errorf("configuration directory %q must be a directory", directory)
	}
	return nil
}

func sanitizeCapabilityError(err error, capabilityURL string) error {
	message := strings.ReplaceAll(err.Error(), capabilityURL, "[REDACTED]")
	return errors.New(message)
}
