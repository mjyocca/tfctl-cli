// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package configversion implements the `config-version` command group.
package configversion

import (
	"github.com/hashicorp/tfctl-cli/internal/pkg/cmd"
	"github.com/hashicorp/tfctl-cli/internal/pkg/heredoc"
	"github.com/hashicorp/tfctl-cli/version"
)

// NewCmdConfigVersion creates the `config-version` command group.
func NewCmdConfigVersion(inv *cmd.Invocation) *cmd.Command {
	c := &cmd.Command{
		Name:      "config-version",
		ShortHelp: "Package and upload Terraform configurations.",
		LongHelp: heredoc.New(inv.IO).Mustf(`
		The {{ template "mdCodeOrBold" "%s config-version" }} command group packages and
		uploads Terraform configurations to HCP Terraform or Terraform Enterprise.
		`, version.Name),
	}
	c.AddChild(NewCmdUpload(inv))
	return c
}
