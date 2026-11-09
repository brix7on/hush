//go:build !windows

package main

import "fmt"

func main() {
	fmt.Println("hush works on Windows only (WASAPI). Build with: GOOS=windows go build ./cmd/hush")
}
