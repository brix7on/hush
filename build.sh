#!/bin/sh
# Тесты и сборка hush.exe под Windows.
set -e
cd "$(dirname "$0")"
mkdir -p dist

echo ">> go vet (linux + windows)"
go vet ./...
GOOS=windows GOARCH=amd64 go vet ./...

echo ">> тесты с детектором гонок"
go test -race -short ./...

echo ">> сборка hush.exe"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w -X main.Version=${VERSION:-dev}" \
	-o dist/hush.exe ./cmd/hush

echo ">> готово: dist/hush.exe"
