# Default configuration (can be overridden: make install VERSION=edge ARCH=arm64)
CONTAINER_NAME ?= lab
IMAGE_NAME ?= groom-lab
VERSION ?= v0.0.1
ARCH ?= amd64
BINARY_NAME = groom
BINARY_URL = https://github.com/etnz/groom/releases/download/$(VERSION)/$(BINARY_NAME)-linux-$(ARCH)
INSTALL_DIR = /usr/local/bin
SERVICE_PATH = /etc/systemd/system/groom.service



REPO_DIR := $(shell pwd)

DOCKER_EXEC := docker exec $(CONTAINER_NAME) bash -c
REPO_URL := https://github.com/etnz/ecrin.git
REPO_DIR := /root/ecrin
BRANCH := master

.PHONY: clean build run enter setup


clean:
	@echo "Cleaning Lab Container..."
	docker rm -f $(CONTAINER_NAME)

build:
	docker build -f Dockerfile -t $(IMAGE_NAME) .

run: clean build
	@echo "Running Lab container $(CONTAINER_NAME)..."
	docker run -d \
		--name $(CONTAINER_NAME) \
		--hostname $(CONTAINER_NAME) \
		--privileged \
		--network host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:ro \
		$(IMAGE_NAME)
	@echo "✅ Lab has started !"


enter:
	docker exec -it $(CONTAINER_NAME) bash


setup:
	@echo "⬇️  Downloading Groom $(VERSION) for $(ARCH) into '$(CONTAINER_NAME)'..."
	docker exec $(CONTAINER_NAME) curl -L -o $(INSTALL_DIR)/$(BINARY_NAME) $(BINARY_URL)
	docker exec $(CONTAINER_NAME) chmod +x $(INSTALL_DIR)/$(BINARY_NAME)

	@echo "⚙️  Copying local groom.service to container..."
	docker cp groom.service $(CONTAINER_NAME):$(SERVICE_PATH)
	@echo "✅ Installation complete."

	
uninstall:
	@echo "🛑 Stopping Groom inside container '$(CONTAINER_NAME)'..."
	docker exec $(CONTAINER_NAME) apt-get remove -y groom
	@echo "👋 Groom uninstalled."