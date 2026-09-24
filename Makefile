# Application name and build directory
APP_NAME = pieceobot
BUILD_DIR = bin
MAIN_PKG = ./cmd/server

.PHONY: all build run clean test build-docker compose-up compose-down logs

all: build

update:
	go mod tidy

build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(APP_NAME) $(MAIN_PKG)

run: build
	./$(BUILD_DIR)/$(APP_NAME)

clean:
	rm -rf $(BUILD_DIR)

test:
	go test ./...

build-docker:
	docker build -t $(APP_NAME) .

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down

logs:
	docker compose logs -f bot
