#!/usr/bin/env bash
# Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
# source code is governed by the Apache v2 license that can be found in the
# LICENSE file.

set -eu

# Validate required environment variables
if [ -z "$GITHUB_REPOSITORY_OWNER" ]; then
	echo "Error: GITHUB_REPOSITORY_OWNER is not set."
	exit 1
fi

if [ -z "$GITHUB_REPOSITORY" ]; then
	echo "Error: GITHUB_REPOSITORY is not set."
	exit 1
fi

if [ -z "$GITHUB_TOKEN" ]; then
	echo "Error: GITHUB_TOKEN is not set."
	exit 1
fi

# Ensure jq is installed
if ! command -v jq &>/dev/null; then
	echo "Error: jq is required but not installed."
	exit 1
fi

OWNER="$GITHUB_REPOSITORY_OWNER"
# GITHUB_REPOSITORY comes as "owner/repo", we need just "repo"
REPO="${GITHUB_REPOSITORY#*/}"

echo "Starting cleanup for image package: $OWNER/$REPO"

CUTOFF_DATE=$(date -d '7 days ago' +%s)
CUTOFF_DATE_FMT=$(date -d @"$CUTOFF_DATE")
echo "Cutoff date: $CUTOFF_DATE_FMT"

DELETED_COUNT=0
FAILED_COUNT=0
KEPT_COUNT=0
DELETED_LIST=()

# Get all package versions with pagination
PAGE=1
while true; do
	echo "Processing page $PAGE..."
	RESPONSE=$(curl -s -H "Accept: application/vnd.github.v3+json" \
		-H "Authorization: token ${GITHUB_TOKEN}" \
		"https://api.github.com/users/$OWNER/packages/container/$REPO/versions?per_page=100&page=$PAGE")
	# Check if response is valid JSON and has items
	LENGTH=$(echo "$RESPONSE" | jq length 2>/dev/null)
	# If jq failed or length is empty/0, we are done
	if [ -z "$LENGTH" ] || [ "$LENGTH" -eq 0 ]; then
		break
	fi
	# Parse versions
	VERSIONS=$(echo "$RESPONSE" | jq -r '.[] | "\(.id)|\(.name)|\(.created_at)" ')
	while IFS='|' read -r VERSION_ID TAG CREATED_AT; do
		[ -z "$VERSION_ID" ] && continue
		# Skip special tags
		if [ "$TAG" = "latest" ]; then
			KEPT_COUNT=$((KEPT_COUNT + 1))
			continue
		fi
		# Skip semantic version tags (v1, v1.2, v1.2.3, etc)
		if [[ "$TAG" =~ ^v[0-9]+ ]]; then
			KEPT_COUNT=$((KEPT_COUNT + 1))
			continue
		fi
		CREATED=$(date -d "$CREATED_AT" +%s)
		CREATED_FMT=$(date -d @"$CREATED")
		if [ "$CREATED" -lt "$CUTOFF_DATE" ]; then
			echo "Deleting image: $TAG (created: $CREATED_FMT)"
			DELETE_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE \
				-H "Authorization: token ${GITHUB_TOKEN}" \
				"https://api.github.com/users/$OWNER/packages/container/$REPO/versions/$VERSION_ID")
			if [ "$DELETE_STATUS" -eq 204 ]; then
				DELETED_COUNT=$((DELETED_COUNT + 1))
				DELETED_LIST+=("| \`$TAG\` | $CREATED_FMT | ✅ Deleted |")
			else
				echo "Failed to delete $TAG (HTTP Status: $DELETE_STATUS)"
				FAILED_COUNT=$((FAILED_COUNT + 1))
				DELETED_LIST+=("| \`$TAG\` | $CREATED_FMT | ❌ Failed ($DELETE_STATUS) |")
			fi
		else
			KEPT_COUNT=$((KEPT_COUNT + 1))
		fi
	done <<<"$VERSIONS"
	PAGE=$((PAGE + 1))
done

echo "Cleanup complete. Deleted: $DELETED_COUNT, Failed: $FAILED_COUNT, Kept/Skipped: $KEPT_COUNT"

# Generate GitHub Step Summary if environment variable is set
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "# Docker Image Cleanup Report"
		echo ""
		echo "**Repository:** $OWNER/$REPO"
		echo "**Date:** $(date)"
		echo "**Cutoff Date:** $CUTOFF_DATE_FMT"
		echo ""
		echo "## Summary"
		echo "| Status | Count |"
		echo "| :--- | :--- |"
		echo "| ✅ Deleted | $DELETED_COUNT |"
		echo "| ❌ Failed | $FAILED_COUNT |"
		echo "| 🛡️ Kept/Skipped | $KEPT_COUNT |"
		echo ""

		if [ ${#DELETED_LIST[@]} -gt 0 ]; then
			echo "## Processed Images"
			echo "| Tag | Created At | Status |"
			echo "| :--- | :--- | :--- |"
			for item in "${DELETED_LIST[@]}"; do
				echo "$item"
			done
		else
			echo "_No images were old enough to be deleted._"
		fi
	} >>"$GITHUB_STEP_SUMMARY"
fi
