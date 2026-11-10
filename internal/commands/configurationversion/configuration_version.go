// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package configurationversion implements the `configuration-version` command group.
package configurationversion

import (
	"github.com/hashicorp/tfctl-cli/internal/pkg/cmd"
	"github.com/hashicorp/tfctl-cli/internal/pkg/heredoc"
	"github.com/hashicorp/tfctl-cli/version"
)

// NewCmdConfigurationVersion creates the `configuration-version` command group.
func NewCmdConfigurationVersion(inv *cmd.Invocation) *cmd.Command {
	c := &cmd.Command{
		Name:      "configuration-version",
		ShortHelp: "Package and upload Terraform configurations.",
		LongHelp: heredoc.New(inv.IO).Mustf(`
		The {{ template "mdCodeOrBold" "%s configuration-version" }} command group packages and
		uploads Terraform configurations to HCP Terraform or Terraform Enterprise.
		`, version.Name),
	}
	c.AddChild(NewCmdUpload(inv))
	return c
}
