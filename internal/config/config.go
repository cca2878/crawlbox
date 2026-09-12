package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Listen      string   `yaml:"listen"`
	DataDir     string   `yaml:"data_dir"`
	Credentials string   `yaml:"credentials"`
	KopiaBinary string   `yaml:"kopia_binary"`
	KopiaConfig string   `yaml:"kopia_config"`
	CacheBytes  int64    `yaml:"cache_bytes"`
	Parallel    int      `yaml:"parallel"`
	Sources     []Source `yaml:"sources"`
}
type Source struct {
	ID               string         `yaml:"id"`
	Name             string         `yaml:"name"`
	Plugin           string         `yaml:"plugin"`
	SHA256           string         `yaml:"sha256"`
	Config           map[string]any `yaml:"config"`
	Hosts            []string       `yaml:"hosts"`
	Schedule         string         `yaml:"schedule"`
	Timezone         string         `yaml:"timezone"`
	Timeout          time.Duration  `yaml:"timeout"`
	MemoryMB         uint32         `yaml:"memory_mb"`
	StagingBytes     int64          `yaml:"staging_bytes"`
	StateBytes       int            `yaml:"state_bytes"`
	DownloadParallel int            `yaml:"download_parallel"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidID(s string) bool { return identifier.MatchString(s) }
func Load(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := yaml.NewDecoder(strings.NewReader(string(b)))
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	return c, c.Validate()
}
func (c *Config) Validate() error {
	if c.CacheBytes == 0 {
		c.CacheBytes = 20 << 30
	}
	if c.CacheBytes < 1 {
		return fmt.Errorf("invalid cache_bytes")
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.Parallel == 0 {
		c.Parallel = 1
	}
	if c.Parallel < 1 {
		return fmt.Errorf("parallel must be positive")
	}
	if c.KopiaBinary == "" {
		c.KopiaBinary = "kopia"
	}
	if c.Credentials == "" || c.KopiaConfig == "" {
		return fmt.Errorf("credentials and kopia_config required")
	}
	c.DataDir, _ = filepath.Abs(c.DataDir)
	seen := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Config == nil {
			s.Config = map[string]any{}
		}
		if !ValidID(s.ID) || seen[s.ID] {
			return fmt.Errorf("invalid or duplicate source id")
		}
		seen[s.ID] = true
		if s.Plugin == "" {
			return fmt.Errorf("plugin required")
		}
		h, e := hex.DecodeString(s.SHA256)
		if e != nil || len(h) != 32 {
			return fmt.Errorf("source %s requires sha256", s.ID)
		}
		if s.Timeout == 0 {
			s.Timeout = 2 * time.Hour
		}
		if s.Timeout <= 0 {
			return fmt.Errorf("invalid timeout")
		}
		if s.MemoryMB == 0 {
			s.MemoryMB = 512
		}
		if s.MemoryMB > 2048 {
			return fmt.Errorf("memory_mb exceeds 2048")
		}
		if s.StagingBytes == 0 {
			s.StagingBytes = 100 << 30
		}
		if s.StagingBytes < 1 {
			return fmt.Errorf("invalid staging_bytes")
		}
		if s.StateBytes == 0 {
			s.StateBytes = 1 << 20
		}
		if s.StateBytes < 1 {
			return fmt.Errorf("invalid state_bytes")
		}
		if s.DownloadParallel == 0 {
			s.DownloadParallel = 4
		}
		if s.DownloadParallel < 1 {
			return fmt.Errorf("invalid download_parallel")
		}
		if s.Timezone == "" {
			s.Timezone = "UTC"
		}
		if _, e = time.LoadLocation(s.Timezone); e != nil {
			return e
		}
		if s.Schedule != "" {
			if _, e = cron.ParseStandard("CRON_TZ=" + s.Timezone + " " + s.Schedule); e != nil {
				return e
			}
		}
		for _, h := range s.Hosts {
			if h == "" || strings.ContainsAny(h, "/*?#@") {
				return fmt.Errorf("hosts must be exact host[:port]")
			}
		}
		if _, e = json.Marshal(s.Config); e != nil {
			return e
		}
	}
	return nil
}
