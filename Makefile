# camera_rpi — see README.md
BIN := bin
PI  ?= pi@raspberrypi.local
# HTTPS: names and IPs the browser uses to reach the Pi (comma-separated).
CERT_DIR   ?= certs
CERT_HOSTS ?= raspberrypi.local,raspberrypi,localhost,127.0.0.1

.PHONY: build build-arm64 build-armv7 test vet run-fake run-fake-py deploy deploy-cert cert clean

build:
	go build -o $(BIN)/camweb ./cmd/camweb
	go build -o $(BIN)/fakecam ./cmd/fakecam

build-arm64:            # 64-bit Raspberry Pi OS
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(BIN)/camweb-arm64 ./cmd/camweb

build-armv7:            # 32-bit Raspberry Pi OS
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o $(BIN)/camweb-armv7 ./cmd/camweb

test:
	go vet ./...
	go test ./...
	python3 -m unittest discover -s camhelper -p 'test_*.py'

vet:
	go vet ./...
	test -z "$$(gofmt -l .)"

# Development without a Pi: Go fake helper replaying a test stream.
run-fake: build
	./$(BIN)/camweb -listen 127.0.0.1:8080 -data ./data -helper-cmd ./$(BIN)/fakecam

# Development without a Pi: the real camhelper.py on top of a fake Picamera2.
run-fake-py: build
	PYTHONPATH=camhelper/fake ./$(BIN)/camweb -listen 127.0.0.1:8080 -data ./data -helper-cmd "python3 camhelper/camhelper.py"

deploy: build-arm64     # copy the binary and the spike script to the Pi
	ssh $(PI) mkdir -p camweb/spike
	scp $(BIN)/camweb-arm64 $(PI):camweb/camweb
	scp spike/m0_spike.py $(PI):camweb/spike/
	ssh $(PI) mkdir -p camweb/camhelper
	scp camhelper/camhelper.py $(PI):camweb/camhelper/

# Private CA + server certificate for HTTPS (WebCodecs needs HTTPS off localhost).
# The CA is created once and reused; install $(CERT_DIR)/ca.crt in your browser/OS.
#   make cert CERT_HOSTS=raspberrypi.local,192.168.1.50
cert:
	go run ./cmd/certgen -dir $(CERT_DIR) -hosts "$(CERT_HOSTS)"

deploy-cert:            # copy the server certificate and key (not the CA key) to the Pi
	scp $(CERT_DIR)/server.crt $(CERT_DIR)/server.key $(PI):camweb/

clean:
	rm -f $(BIN)/camweb $(BIN)/fakecam $(BIN)/camweb-arm64 $(BIN)/camweb-armv7
