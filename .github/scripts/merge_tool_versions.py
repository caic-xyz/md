#!/usr/bin/env python3
# Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
# source code is governed by the Apache v2 license that can be found in the
# LICENSE file.

# Merges tool version reports from amd64 and arm64 architectures.
import argparse
import datetime
import re
import sys
from pathlib import Path


def parse_markdown_table(file_path):
    """Parses a markdown table into a dictionary of {tool: version}."""
    tools = {}
    path = Path(file_path)
    if not path.exists():
        print(f"Warning: {file_path} not found.")
        return tools

    with open(file_path, "r", encoding="utf-8") as f:
        for line in f:
            # Match markdown table row: | Tool | Version |
            match = re.match(r"^\|\s*(.*?)\s*\|\s*(.*?)\s*\|$", line)
            if match:
                tool = match.group(1).strip()
                version = match.group(2).strip()
                # Skip header and separator
                if tool.lower() in ["tool", ":---"]:
                    continue
                tools[tool] = version
    return tools


def main():
    """Main function to merge tool version reports."""
    parser = argparse.ArgumentParser(description="Merge tool version reports from different architectures.")
    parser.add_argument("--amd64", required=False, help="Path to amd64 tool versions report")
    parser.add_argument("--arm64", required=False, help="Path to arm64 tool versions report")
    parser.add_argument("--output", required=False, help="Path to save the unified output report")

    args = parser.parse_args()

    amd64_path = args.amd64
    arm64_path = args.arm64

    # At least one architecture must be provided
    if not amd64_path and not arm64_path:
        print("Error: At least one of --amd64 or --arm64 must be provided.", file=sys.stderr)
        sys.exit(1)

    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")

    amd64_tools = parse_markdown_table(amd64_path) if amd64_path else {}
    arm64_tools = parse_markdown_table(arm64_path) if arm64_path else {}

    # Get all unique tools, maintaining order from amd64 then arm64
    all_tools = []
    seen = set()
    for tools in [amd64_tools, arm64_tools]:
        for tool in tools:
            if tool not in seen:
                all_tools.append(tool)
                seen.add(tool)

    # Generate unified tool versions content
    content = [
        "# Tool Versions",
        f"Generated on {now}",
        "",
    ]
    # Determine which columns to include based on provided architectures
    if amd64_path and arm64_path:
        # Both architectures provided
        content.extend(
            [
                "| Tool | amd64 | arm64 | ",
                "| :--- | :--- | :--- |",
            ]
        )
        for tool in all_tools:
            v_amd64 = amd64_tools.get(tool, "Not found")
            v_arm64 = arm64_tools.get(tool, "Not found")
            content.append(f"| {tool} | {v_amd64} | {v_arm64} |")
    elif amd64_path:
        # Only amd64 provided
        content.extend(
            [
                "| Tool | amd64 |",
                "| :--- | :--- |",
            ]
        )
        for tool in all_tools:
            v_amd64 = amd64_tools.get(tool, "Not found")
            content.append(f"| {tool} | {v_amd64} |")
    elif arm64_path:
        # Only arm64 provided
        content.extend(
            [
                "| Tool | arm64 |",
                "| :--- | :--- |",
            ]
        )
        for tool in all_tools:
            v_arm64 = arm64_tools.get(tool, "Not found")
            content.append(f"| {tool} | {v_arm64} |")
    unified_content = "\n".join(content) + "\n"

    # Save the unified report
    if args.output:
        output_path = Path(args.output)
        output_path.parent.mkdir(parents=True, exist_ok=True)
        output_path.write_text(unified_content, encoding="utf-8")
    print(unified_content)


if __name__ == "__main__":
    sys.exit(main())
