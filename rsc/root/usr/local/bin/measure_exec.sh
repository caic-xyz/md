#!/bin/bash
# Wrapper script to measure execution time of a command and log it to a markdown table.

set -e

if [ "$#" -lt 2 ]; then
  echo "Usage: $0 <Label> <Command> [Args...]"
  exit 1
fi

LABEL="$1"
CMD_BASENAME=$(basename "$2")
shift

START=$(date +%s)

# Execute the command
"$@"

END=$(date +%s)
ELAPSED=$((END - START))

# Format as mm:ss
MIN=$((ELAPSED / 60))
SEC=$((ELAPSED % 60))
DURATION=$(printf "%dm%02ds" $MIN $SEC)

OUTPUT_FILE="/var/log/build_timings.md"
flock "$OUTPUT_FILE" -c "echo \"| $LABEL | $CMD_BASENAME | $DURATION |\" >> \"$OUTPUT_FILE\""
