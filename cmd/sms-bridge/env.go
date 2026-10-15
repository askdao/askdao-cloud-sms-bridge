// [INPUT]: os environment, an optional .env file in the working directory
// [OUTPUT]: loadDotEnv, config, loadConfig
// [POS]: cmd/sms-bridge's configuration reader; main.go calls loadDotEnv then loadConfig
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv sets KEY=VALUE lines from path into the environment without overriding variables
// that are already set. Blank lines and lines starting with # are skipped; one pair of matching
// surrounding quotes is removed from a value. A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok || key == "" {
			return fmt.Errorf("%s line %d: expected KEY=VALUE", path, n) // the line itself may hold a secret
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, val); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

type config struct {
	SessionFile string
}

func loadConfig() config {
	return config{SessionFile: envOr("BRIDGE_SESSION_FILE", "session.json")}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
