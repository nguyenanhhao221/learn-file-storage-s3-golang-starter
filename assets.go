package main

import (
	"os"
	"strings"
)

func (cfg apiConfig) ensureAssetsDir() error {
	if _, err := os.Stat(cfg.assetsRoot); os.IsNotExist(err) {
		return os.Mkdir(cfg.assetsRoot, 0755)
	}
	return nil
}

func getFileExtension(mediaType string) string {
	parts := strings.Split(mediaType, "/")
	if len(parts) != 2 {
		return "bin"
	}
	return parts[1]
}
