package main

import (
	_ "embed"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// The broker's version lives in the VERSION file (one line, e.g. 0.2.0), embedded at build time.
// It's shown by -version, in GET /v1/info, on the website and in the start-up log.
// A release tag must be "v" + this (the release workflow checks).
//
//go:embed VERSION
var versionFile string

var version = strings.TrimSpace(versionFile)

// Config holds every setting. Each one can come from a flag or an environment variable
// (the flag wins). See docs/operations.md.
type Config struct {
	Addr         string        // where to listen, e.g. ":8080"
	DBPath       string        // SQLite file
	KeyFile      string        // server signing key (created on first start if missing)
	MaxDays      int           // how many days of history a pair may hold (the "60 days" rule)
	StaleDays    int           // a pair not written for this many days is deleted
	CleanupEvery time.Duration // how often the clean-up job runs
	MaxPairs     int           // refuse new pairs above this many rows (protects the disk)
	Readers      int           // size of the read-only connection pool
	RateLimit    bool          // per-IP rate limits (false: leave all limiting to a reverse proxy)
	ClientIP     string        // header with the client IP, set by a trusted proxy (X-Forwarded-For, CF-Connecting-IP, ...)
	Proxies      []*net.IPNet  // peers whose ClientIP header is believed (-trusted-proxies)
	Password     string        // optional: every API call must send it (self-hosted, private brokers). "" = open
	DownloadURL  string        // "Download" button on the website
	SourceURL    string        // source code link in the website footer
	AuthorName   string
	AuthorURL    string
}

func loadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("better-red-broker", flag.ContinueOnError)
	c := Config{}
	fs.StringVar(&c.Addr, "addr", env("BR_ADDR", ":8080"), "listen address")
	fs.StringVar(&c.DBPath, "db", env("BR_DB", "data/broker.db"), "SQLite database file")
	fs.StringVar(&c.KeyFile, "key-file", env("BR_KEY_FILE", "data/server.key"), "server signing key file (created if missing)")
	fs.IntVar(&c.MaxDays, "max-days", envInt("BR_MAX_DAYS", 60), "days of history kept per pair")
	fs.IntVar(&c.StaleDays, "stale-days", envInt("BR_STALE_DAYS", 10), "delete pairs not updated for this many days")
	fs.DurationVar(&c.CleanupEvery, "cleanup-every", envDuration("BR_CLEANUP_EVERY", time.Hour), "how often to run the clean-up job")
	fs.IntVar(&c.MaxPairs, "max-pairs", envInt("BR_MAX_PAIRS", 100_000), "maximum number of pairs stored")
	fs.IntVar(&c.Readers, "readers", envInt("BR_READERS", 8), "read-only database connections")
	fs.BoolVar(&c.RateLimit, "rate-limit", envBool("BR_RATE_LIMIT", true), "per-IP rate limits (turn off only if the reverse proxy limits instead)")
	fs.StringVar(&c.ClientIP, "client-ip-header", env("BR_CLIENT_IP_HEADER", "X-Forwarded-For"), "header with the client IP, believed only from -trusted-proxies (X-Forwarded-For, CF-Connecting-IP, X-Real-IP)")
	passwordFile := fs.String("password-file", env("BR_PASSWORD_FILE", ""), "file holding the API password (instead of BR_PASSWORD)")
	proxies := fs.String("trusted-proxies", env("BR_TRUSTED_PROXIES", "loopback"), "comma-separated proxy addresses/CIDRs, 'loopback', 'private' or 'none'")
	fs.StringVar(&c.DownloadURL, "download-url", env("BR_DOWNLOAD_URL", "https://github.com/bRuttaZz/better-red/releases"), "website download link")
	fs.StringVar(&c.SourceURL, "source-url", env("BR_SOURCE_URL", "https://github.com/bRuttaZz/better-red"), "website source link")
	fs.StringVar(&c.AuthorName, "author", env("BR_AUTHOR", "bruttazz"), "website author name")
	fs.StringVar(&c.AuthorURL, "author-url", env("BR_AUTHOR_URL", "https://brutt.site"), "website author link")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	var err error
	if c.Proxies, err = parseProxies(*proxies); err != nil {
		return c, err
	}
	// The password never comes from a flag: command lines are visible to every user (ps).
	if c.Password, err = loadPassword(os.Getenv("BR_PASSWORD"), *passwordFile); err != nil {
		return c, err
	}
	switch {
	case c.MaxDays < 1 || c.MaxDays > 366:
		return c, fmt.Errorf("max-days must be 1..366")
	case c.StaleDays < 1:
		return c, fmt.Errorf("stale-days must be at least 1")
	case c.CleanupEvery < time.Minute:
		return c, fmt.Errorf("cleanup-every must be at least 1m")
	case c.MaxPairs < 1:
		return c, fmt.Errorf("max-pairs must be at least 1")
	case c.Readers < 1 || c.Readers > 256:
		return c, fmt.Errorf("readers must be 1..256")
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// loadPassword reads the optional API password from BR_PASSWORD or a file (trailing newline
// ignored). 1–128 printable ASCII characters without spaces: it travels in an HTTP header and is
// typed into the app as "password@domain".
func loadPassword(fromEnv, file string) (string, error) {
	p := fromEnv
	if file != "" {
		if p != "" {
			return "", fmt.Errorf("set BR_PASSWORD or -password-file, not both")
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("password file: %w", err)
		}
		p = strings.TrimRight(string(raw), "\r\n")
	}
	if p == "" {
		return "", nil
	}
	if !validPassword(p) {
		return "", fmt.Errorf("the password must be 1-128 printable ASCII characters, no spaces")
	}
	return p, nil
}

func validPassword(p string) bool {
	if len(p) < 1 || len(p) > 128 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x21 || p[i] > 0x7e {
			return false
		}
	}
	return true
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// parseProxies reads -trusted-proxies: IPs or CIDRs, plus the shortcuts "loopback" (this machine),
// "private" (loopback and private networks: containers, LANs, VPNs) and "none".
func parseProxies(s string) ([]*net.IPNet, error) {
	shortcuts := map[string][]string{
		"none":     {},
		"loopback": {"127.0.0.0/8", "::1/128"},
		"private":  {"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7"},
	}
	var out []*net.IPNet
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		cidrs, ok := shortcuts[strings.ToLower(item)]
		if !ok {
			if !strings.Contains(item, "/") {
				if ip := net.ParseIP(item); ip != nil && ip.To4() != nil {
					item += "/32"
				} else {
					item += "/128"
				}
			}
			cidrs = []string{item}
		}
		for _, c := range cidrs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, fmt.Errorf("trusted-proxies: %q is not an IP, a CIDR, loopback, private or none", item)
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
