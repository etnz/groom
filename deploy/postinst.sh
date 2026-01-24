#!/bin/sh
set -e

# Reload systemd to recognize the new unit file
systemctl daemon-reload

# Enable and start the service immediately
systemctl enable --now groom.service

# Health Check Loop (Wait up to 10 seconds)
# This ensures dpkg fails if the service crashes on boot
echo "Waiting for Groom to become healthy..."
for i in $(seq 1 10); do
    if systemctl is-active --quiet groom.service; then
        # Optional: Deep check via HTTP if curl is available
        # We use -s (silent) and -f (fail on HTTP error)
        if command -v curl >/dev/null 2>&1; then
            if curl -s -f http://localhost:8080/health >/dev/null 2>&1; then
                echo "✅ Groom is running and healthy!"
                exit 0
            fi
        else
            # Fallback if curl is missing: trust systemd status
            echo "✅ Groom service is active."
            exit 0
        fi
    fi
    sleep 1
done

echo "❌ Groom failed to start or is unhealthy."
# Show logs to help debugging
journalctl -u groom.service -n 20 --no-pager
exit 1