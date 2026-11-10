// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package configurationversion

import (
	"github.com/dustin/go-humanize"

	"github.com/hashicorp/tfctl-cli/internal/pkg/format"
)

type uploadDisplayer struct {
	result UploadResult
}

func (d uploadDisplayer) DefaultFormat() format.Format { return format.Pretty }
func (d uploadDisplayer) Payload() any                 { return d.result }
func (d uploadDisplayer) FieldTemplates() []format.Field {
	return []format.Field{
		format.NewField("ID", "{{ .ID }}"),
		format.NewField("Workspace ID", "{{ .WorkspaceID }}"),
		format.NewField("Status", "{{ .Status }}"),
		format.NewField("Files", "{{ .Files }}"),
		format.NewField("Compressed Size", "{{ .CompressedSizeDisplay }}"),
		format.NewField("Speculative", "{{ .Speculative }}"),
		format.NewField("Provisional", "{{ .Provisional }}"),
		format.NewField("Auto Queue Run", "{{ .AutoQueueRun }}"),
	}
}

// CompressedSizeDisplay returns a human-readable compressed archive size.
func (r UploadResult) CompressedSizeDisplay() string {
	return humanize.Bytes(uint64(r.CompressedSize))
}
