#!/bin/sh
# Demo check for 07-ephemeral and 08-pipeline: prints the distro and kernel, keeps a report.
set -e
. /etc/os-release
echo "$PRETTY_NAME, kernel $(uname -r)"
echo "$ID ok" > report.txt
