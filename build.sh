#!/bin/sh
# Тесты и сборка hush.exe под Windows.
set -e
cd "$(dirname "$0")"

echo ">> тесты"
cargo test --release

echo ">> сборка hush.exe"
HUSH_VERSION="${VERSION:-dev}" cargo build --release --target x86_64-pc-windows-gnu
mkdir -p dist
cp target/x86_64-pc-windows-gnu/release/hush.exe dist/hush.exe

echo ">> готово: dist/hush.exe"
