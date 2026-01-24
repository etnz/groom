#!/bin/sh
set -e

# Stop the service before removing the package or upgrading
systemctl stop groom.service || true
systemctl disable groom.service || true

exit 0