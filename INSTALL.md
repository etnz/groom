# Installing Groom

This guide explains how to install **Groom** on your bare-metal Linux device (e.g., Raspberry Pi) as a background service.

## Prerequisites

* Root/Sudo access to the device.

* Internet connection.

## 1. Download and Install Binary

Download the latest stable release for your architecture (adjust `v0.0.1` to the latest version found on the GitHub Releases page).

```bash
# 1. Download the binary to /usr/local/bin
sudo curl -L -o /usr/local/bin/groom \
  [https://github.com/etnz/groom/releases/download/v0.0.1/groom-linux-arm64](https://github.com/etnz/groom/releases/download/v0.0.1/groom-linux-arm64)

# 2. Make it executable
sudo chmod +x /usr/local/bin/groom
```

## 2. Create Systemd Service

Create the service definition file to let the system manage Groom automatically.

```bash
sudo tee /etc/systemd/system/groom.service > /dev/null <<EOF
[Unit]
Description=Groom Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/groom
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF
```

## 3. Start the Service

Reload systemd to pick up the new file, then enable and start Groom.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now groom
```

## 4. Verify

Check that Groom is running:

```bash
systemctl status groom
```

You should see "Active: active (running)".

To view the logs in real-time and confirm mDNS advertising:

```bash
sudo journalctl -u groom -f
```

You should see logs indicating `Groom's mDNS Advertising active`.