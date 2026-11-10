package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// loadEnvFile reads KEY=VALUE lines from wend.env, next to the binary
// or in the working directory, or from ROUTER_ENV_FILE. Variables already
// set in the environment win. Returns the file used, empty when none.
func loadEnvFile() string {
	var paths []string
	if p := os.Getenv("ROUTER_ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "wend.env"))
	}
	paths = append(paths, "wend.env")
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if _, set := os.LookupEnv(k); !set && v != "" {
				os.Setenv(k, v)
			}
		}
		return p
	}
	return ""
}
