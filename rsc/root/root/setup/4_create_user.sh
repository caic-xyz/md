#!/bin/bash
# Create the unprivileged user account.
set -euo pipefail
echo "- $0"

# Create user "user"
useradd -ms /bin/bash user
