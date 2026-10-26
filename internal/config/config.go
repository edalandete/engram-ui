package config

import (
	"flag"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// EngramURL is the base URL for the engram HTTP API.
	EngramURL string

	// EngramToken is the optional bearer token (matches ENGRAM_HTTP_TOKEN on the engram side).
	EngramToken string

	// CloudToken is the optional bearer token for the engram cloud server. When
	// set, it is forwarded as ENGRAM_CLOUD_TOKEN to `engram sync` CLI calls so a
	// bearer-authenticated cloud works without persisting the token to disk.
	// For a local insecure cloud (ENGRAM_CLOUD_INSECURE_NO_AUTH) leave it empty.
	CloudToken string

	// ListenAddr is where the engram-ui web server binds.
	ListenAddr string

	// SpawnEngram controls whether engram-ui starts engram serve as a subprocess.
	// When false, an external engram serve is assumed.
	SpawnEngram bool

	// OpenBrowser controls whether the OS browser is launched on start.
	OpenBrowser bool
}

func FromFlags() *Config {
	c := &Config{}
	flag.StringVar(&c.EngramURL, "engram-url", envOr("ENGRAM_URL", "http://127.0.0.1:7437"), "engram HTTP API base URL")
	flag.StringVar(&c.EngramToken, "engram-token", os.Getenv("ENGRAM_HTTP_TOKEN"), "bearer token for engram (optional)")
	flag.StringVar(&c.CloudToken, "cloud-token", os.Getenv("ENGRAM_CLOUD_TOKEN"), "bearer token for engram cloud sync (optional)")
	flag.StringVar(&c.ListenAddr, "listen", envOr("ENGRAM_UI_LISTEN", "127.0.0.1:7438"), "web UI bind address")
	flag.BoolVar(&c.SpawnEngram, "spawn-engram", envBool("ENGRAM_UI_SPAWN", true), "start an embedded engram subprocess")
	flag.BoolVar(&c.OpenBrowser, "open-browser", envBool("ENGRAM_UI_OPEN", true), "open the OS browser on start")
	return c
}

// envBool reads a boolean environment variable the way people write them.
//
// This used to be `envOr(key, "true") == "true"`, an exact string comparison,
// so ENGRAM_UI_SPAWN=1 — the most natural way to write "yes" — evaluated to
// false and turned the subprocess OFF. The user sets a variable to enable
// something and disables it instead, then gets "engram not reachable", a
// message that never mentions the variable. Same for TRUE, True and yes.
//
// strconv.ParseBool covers 1/t/T/TRUE/true/True and their negatives. Anything
// it does not recognise falls back to the default rather than silently
// meaning false.
func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// "yes"/"no" are common enough to be worth understanding.
		switch strings.ToLower(v) {
		case "yes", "on":
			return true
		case "no", "off":
			return false
		}
		return fallback
	}
	return b
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
