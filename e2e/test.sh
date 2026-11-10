#!/usr/bin/env bash
# Copyright IBM Corp. 2026
# SPDX-License-Identifier: MPL-2.0


set -euo pipefail

# End-to-end test for tfctl
#
# Runs dist/tfctl through some basic test cases. Presumes the default profile
# is already configured. 'setup' creates $organization and all cases should
# use it.
# 
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tfctl_bin="${TFCTL_BIN:-$root_dir/dist/tfctl}"
run_id="${GITHUB_RUN_ID:-$(date +%s)}-${GITHUB_RUN_ATTEMPT:-$RANDOM}"
organization="tfctl-e2e-${run_id}"
organization_created=false

teardown() {
  local status=$?

  if [ "$organization_created" = true ]; then
    "$tfctl_bin" harness exec --allow-delete=organizations -- \
      "$tfctl_bin" api "/organizations/$organization" -X DELETE || status=1
  fi

  exit "$status"
}
trap teardown EXIT

setup() {
  if [ ! -x "$tfctl_bin" ]; then
    printf 'tfctl binary not found at %s\n' "$tfctl_bin" >&2
    exit 1
  fi

  printf 'Creating organization %s\n' "$organization"
  "$tfctl_bin" api "/organizations" -X POST -a "name=$organization" -a "email=tfctl-e2e@example.com" --quiet
  organization_created=true
}

run_case() {
  local name=$1
  printf '\n=== %s ===\n' "$name"
  "$name"
}

assert_contains() {
  local value=$1
  local expected=$2

  case "$value" in
    *"$expected"*) ;;
    *)
      printf 'expected output to contain %q:\n%s\n' "$expected" "$value" >&2
      return 1
      ;;
  esac
}

assert_not_contains() {
  local value=$1
  local unexpected=$2

  case "$value" in
    *"$unexpected"*)
      printf 'expected output not to contain %q:\n%s\n' "$unexpected" "$value" >&2
      return 1
      ;;
    *) ;;
  esac
}

create_auto_apply_workspace() {
  local workspace="tfctl-e2e-$RANDOM"

  "$tfctl_bin" create workspace --organization "$organization" --jq '.data.id' \
    -a "name=$workspace" -a auto-apply=true
}

upload_configuration() {
  local workspace_id=$1
  local directory=$2

  printf 'Uploading configuration\n' >&2
  "$tfctl_bin" config-version upload "$workspace_id" \
    --directory "$directory" \
    --jq '.id'
}

case_get_formats() {
  local workspace workspace_id output
  workspace="tfctl-e2e-formats-$RANDOM"
  workspace_id="$("$tfctl_bin" create workspace --organization "$organization" --jq '.data.id' -a "name=$workspace")"

  output="$("$tfctl_bin" get workspaces --organization "$organization")"
  assert_contains "$output" "ID"
  output="$("$tfctl_bin" get workspaces --organization "$organization" --json)"
  assert_contains "$output" "\"$workspace\""
  output="$("$tfctl_bin" get workspaces --organization "$organization" --markdown)"
  assert_contains "$output" "$workspace"

  output="$("$tfctl_bin" get workspace "$workspace_id")"
  assert_contains "$output" "$workspace"
  output="$("$tfctl_bin" get workspace "$workspace_id" --json)"
  assert_contains "$output" "\"id\": \"$workspace_id\""
  output="$("$tfctl_bin" get workspace "$workspace_id" --markdown)"
  assert_contains "$output" "$workspace"
}

case_dry_run_is_no_op() {
  local workspace output
  workspace="tfctl-e2e-dry-run-$RANDOM"

  output="$("$tfctl_bin" create workspace --organization "$organization" -a "name=$workspace" --dry-run 2>&1)"
  assert_contains "$output" "would send POST request"

  output="$("$tfctl_bin" get workspaces --organization "$organization" --json)"
  assert_not_contains "$output" "$workspace"
}

case_quiet_minimizes_output() (
  local workspace stdout stderr
  workspace="tfctl-e2e-quiet-$RANDOM"
  stdout="$(mktemp)"
  stderr="$(mktemp)"
  trap 'rm -f "$stdout" "$stderr"' EXIT

  "$tfctl_bin" create workspace --organization "$organization" -a "name=$workspace" --quiet >"$stdout" 2>"$stderr"
  [ ! -s "$stdout" ]
  [ ! -s "$stderr" ]
)

case_harness_install() (
  local temp_dir skill_path
  temp_dir="$(mktemp -d)"
  skill_path="$temp_dir/.agents/skills/tfctl/SKILL.md"
  trap 'rm -rf "$temp_dir"' EXIT

  (
    cd "$temp_dir"
    "$tfctl_bin" harness install opencode
  )
  [ -s "$skill_path" ]
)

case_profile_display() {
  local output
  output="$("$tfctl_bin" profile display --json)"
  assert_contains "$output" "\"Name\":"
  assert_not_contains "$output" "token"
}

case_create_and_apply_workspace() (
  local configuration_id run_count workspace_id

  printf 'Creating auto-apply workspace\n'
  workspace_id="$(create_auto_apply_workspace)"
  configuration_id="$(upload_configuration "$workspace_id" "$root_dir/e2e")"

  printf 'Starting and waiting for the auto-apply run\n'
  "$tfctl_bin" run start "$workspace_id" \
    --configuration-version "$configuration_id" \
    --wait \
    --timeout 20m

  run_count="$("$tfctl_bin" api "/workspaces/$workspace_id/runs" --jq '.data | length')"
  if [ "$run_count" -ne 1 ]; then
    printf 'expected exactly one run for workspace %s, got %s\n' "$workspace_id" "$run_count" >&2
    return 1
  fi
)

setup
run_case case_get_formats
run_case case_dry_run_is_no_op
run_case case_quiet_minimizes_output
run_case case_harness_install
run_case case_profile_display
run_case case_create_and_apply_workspace
