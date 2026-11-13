package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadEnvFile sets KEY=VALUE pairs from path without overriding variables
// already set in the environment. A missing file is not an error.
// Supports blank lines, # comments, an optional "export " prefix and quoted values.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// warnLegacyEnv flags LOCAL_KANBAN_* variables (env or .env) from before the rename: they are ignored,
// so e.g. a sync URL under the old name would silently leave this machine local-only.
func warnLegacyEnv() {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "LOCAL_KANBAN_") {
			fmt.Fprintf(os.Stderr, "warning: %s is ignored since the rename; use AGENT_KANBAN_%s\n", k, strings.TrimPrefix(k, "LOCAL_KANBAN_"))
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
