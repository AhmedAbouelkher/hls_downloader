# https://stackoverflow.com/questions/2057689/how-does-make-app-know-default-target-to-build-if-no-target-is-specified
.DEFAULT_GOAL := build

.PHONY: default
default: build ;

build_windows:
	@echo "Building windows exe..."
	GOOS=windows go build -ldflags "-s -w" -o ./exec/hls_downloader.exe main.go utils.go
	@echo "Done."

build_linux:
	@echo "Building linux exe..."
	GOOS=linux go build -ldflags "-s -w" -o ./exec/hls_downloader_linux main.go utils.go
	@echo "Done."

build_mac:
	@echo "Building mac exe..."
	GOOS=darwin go build -ldflags "-s -w" -o ./exec/hls_downloader main.go utils.go
	@echo "Done."

build_macos: build_mac
	cp ./exec/hls_downloader /usr/local/bin/

build: build_windows build_linux build_mac
