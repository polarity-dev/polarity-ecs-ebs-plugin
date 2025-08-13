PLUGIN_NAME=polarity-ecs-ebs-plugin
BINARY_NAME=polarity-ecs-ebs-plugin
SOCK_NAME=pl-ebs
DEV_SOCK_PATH=./$(SOCK_NAME).sock
SOCK_PATH=/run/docker/plugins/$(SOCK_NAME).sock
BUILD_DIR=build
ROOTFS_DIR=$(BUILD_DIR)/rootfs
BIN_DIR=$(ROOTFS_DIR)/bin

.PHONY: all tar clean build

build: clean generate-config
	@echo "Building Go binary..."
	go clean && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/plugin
plugin: build
	@echo "Creating plugin"
	docker plugin create polarity-ecs-ebs-plugin:latest ./build
	docker plugin enable polarity-ecs-ebs-plugin:latest

tar: docker-build
	@echo "Creating plugin tarball..."
	tar -czf $(PLUGIN_NAME).tar.gz -C $(BUILD_DIR) .

clean:
	@echo "Cleaning up..."
	docker plugin disable polarity-ecs-ebs-plugin:latest || true
	docker plugin rm polarity-ecs-ebs-plugin:latest || true
	rm -rf $(BUILD_DIR) *.tar.gz *.tar plugin.log
# 	sudo rm $(SOCK_PATH) || true
buildx-amd64: clean generate-config
	docker buildx build --platform linux/amd64 -t plx86 --load .
	DOCKER_ID=$$(docker create plx86); \
	docker export $$DOCKER_ID | tar -x -C ./build/rootfs; \
	docker rm $$DOCKER_ID
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
tar-amd64: buildx-amd64
	@echo "Creating plugin tarball for amd64..." && tar -czf $(PLUGIN_NAME)-amd64.tar.gz -C $(BUILD_DIR) .
debug-tar-amd64: debug-build-amd64
	@echo "Creating plugin tarball for amd64..." && tar -czf $(PLUGIN_NAME)-amd64-debug.tar.gz -C $(BUILD_DIR) .
debug-generate-config: clean
	@echo "Generating config.json..."
	mkdir -p $(BUILD_DIR)/rootfs
	@echo '{"description":"Polarity EBS plugin for ECS","entrypoint":["/bin/$(BINARY_NAME)"], "interface":{"types":["docker.volumedriver/1.0"],"socket":"$(SOCK_NAME).sock"},"mounts":[{"source":"/dev","destination":"/dev","type":"bind","options":["rbind"]},{"source":"/var/log","destination":"/logging","type":"bind","options":["rbind"]}],"propagatedMount":"/mnt","network":{"type":"host"},"linux":{"allowAllDevices":true,"capabilities":["CAP_SYS_ADMIN"]}}' > $(BUILD_DIR)/config.json
generate-config: clean
	@echo "Generating config.json..."
	mkdir -p $(BUILD_DIR)/rootfs
	@echo '{"description":"Polarity EBS plugin for ECS","entrypoint":["/bin/$(BINARY_NAME)"], "interface":{"types":["docker.volumedriver/1.0"],"socket":"$(SOCK_NAME).sock"},"mounts":[{"source":"/dev","destination":"/dev","type":"bind","options":["rbind"]}],"propagatedMount":"/mnt","network":{"type":"host"},"linux":{"allowAllDevices":true,"capabilities":["CAP_SYS_ADMIN"]}}' > $(BUILD_DIR)/config.json
debug-build-amd64: debug-generate-config
	@echo "Building amd64 Docker image for debugging..."
	docker buildx build --platform linux/amd64 --build-arg DEBUG=true -t plx86 --load .
	DOCKER_ID=$$(docker create plx86); \
	docker export $$DOCKER_ID | tar -x -C ./build/rootfs; \
	docker rm $$DOCKER_ID
