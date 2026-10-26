.PHONY: all build run test vet clean submodules build-engram bump-engram release

all: build

submodules:
	git submodule update --init --recursive

build-engram: submodules
	pwsh -NoProfile -File ./build-engram.ps1

build: submodules
	pwsh -NoProfile -File ./build.ps1

run:
	go run ./cmd/engram-ui

test:
	go test ./...

vet:
	go vet ./...

bump-engram:
	pwsh -NoProfile -File ./bump-engram.ps1

release:
	pwsh -NoProfile -File ./release.ps1

clean:
	pwsh -NoProfile -Command "Remove-Item -Recurse -Force -ErrorAction SilentlyContinue bin, internal/launcher/embedded/engram, internal/launcher/embedded/engram.exe, internal/launcher/embedded/engram.exe~"
