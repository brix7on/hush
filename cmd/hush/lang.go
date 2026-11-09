package main

import (
	"os"
	"strings"
	"sync"
)

// Version подставляется при сборке релиза.
var Version = "dev"

const appName = "hush"

var (
	langOnce sync.Once
	langRU   bool
)

func isRussianUI() bool {
	langOnce.Do(func() {
		if v := os.Getenv("HUSH_LANG"); v != "" {
			langRU = strings.HasPrefix(strings.ToLower(v), "ru")
			return
		}
		langRU = systemRussian()
	})
	return langRU
}

// L выбирает строку под язык системы.
func L(ru, en string) string {
	if isRussianUI() {
		return ru
	}
	return en
}
