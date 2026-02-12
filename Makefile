BUILD_DIR := build

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

OUTPUT_DIR := $(BUILD_DIR)/$(GOOS)-$(GOARCH)

INITD_BIN := $(OUTPUT_DIR)/initd
SYSTEMCTL_BIN := $(OUTPUT_DIR)/systemctl

.PHONY: build clean

build:
	@mkdir -p $(OUTPUT_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(INITD_BIN) ./cmd/initd
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(SYSTEMCTL_BIN) ./cmd/systemctl
	@echo "Build completed for $(GOOS)/$(GOARCH)."

clean:
	rm -rf $(BUILD_DIR)/*
	@echo "Clean completed."
