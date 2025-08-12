PLUGIN_NAME=polarity-ebs-plugin
BINARY_NAME=polarity-ebs-plugin
SOCK_NAME=pl-ebs
DEV_SOCK_PATH=./$(SOCK_NAME).sock
SOCK_PATH=/run/docker/plugins/$(SOCK_NAME).sock
BUILD_DIR=build
ROOTFS_DIR=$(BUILD_DIR)/rootfs
BIN_DIR=$(ROOTFS_DIR)/bin

.PHONY: all tar clean build generate-config create-plugin

build: clean generate-config
	@echo "Building Go binary..."
	go clean && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/plugin

generate-config:
	@echo "Generating config.json..."
	mkdir -p $(BUILD_DIR)/rootfs
	@echo '{"description":"Polarity EBS plugin for ECS","entrypoint":["/bin/$(BINARY_NAME)"], "interface":{"types":["docker.volumedriver/1.0"],"socket":"$(SOCK_NAME).sock"},"mounts":[{"source":"/dev","destination":"/dev","type":"bind","options":["rbind"]}],"propagatedMount":"/mnt","network":{"type":"host"},"linux":{"allowAllDevices":true,"capabilities":["CAP_SYS_ADMIN"]}}' > $(BUILD_DIR)/config.json

plugin: build
	@echo "Creating plugin"
	docker plugin create polarity-ebs-plugin:latest ./build
	docker plugin enable polarity-ebs-plugin:latest

tar: docker-build
	@echo "Creating plugin tarball..."
	tar -cvf $(PLUGIN_NAME).tar -C $(BUILD_DIR) .

clean:
	@echo "Cleaning up..."
	docker plugin disable polarity-ebs-plugin:latest || true
	docker plugin rm polarity-ebs-plugin:latest || true
	rm -rf $(BUILD_DIR) $(PLUGIN_NAME).tar plugin.log
	sudo rm $(SOCK_PATH) || true

dev:
	@echo "Running with go run and default params..."
	SOCK_PATH=$(DEV_SOCK_PATH) REGION=empty AVAILABILITY_ZONE=empty INSTANCE_ID=empty go run cmd/plugin/main.go

health-check:
	@echo "Checking health..."
	curl -H "Content-Type: application/json" -XPOST -d "{}" --unix-socket $(DEV_SOCK_PATH) http:/localhost/health
docker-build: clean generate-config
	@echo "Building Docker image..."
	mkdir -p ./build/rootfs && docker build -t $(PLUGIN_NAME) .
	DOCKER_ID=$$(docker create $(PLUGIN_NAME)); \
	docker export $$DOCKER_ID | tar -x -C ./build/rootfs; \
	docker rm $$DOCKER_ID
