BUILD_DIR := build

INITD_BIN := $(BUILD_DIR)/initd
SYSTEMCTL_BIN := $(BUILD_DIR)/systemctl

.PHONY: build clean

build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(INITD_BIN) ./cmd/initd
	go build -o $(SYSTEMCTL_BIN) ./cmd/systemctl
	@echo "Build completed."

clean:
	rm -rf $(BUILD_DIR)/*
	@echo "Clean completed."
