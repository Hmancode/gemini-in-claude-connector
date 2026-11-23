package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config lives at %APPDATA%\gemini-bridge\config.json on Windows.
type Config struct {
	APIKey         string   `json:"api_key,omitempty"`
	DefaultModel   string   `json:"default_model,omitempty"`    // "flash" (default), "pro", or an exact model id
	FlashModel     string   `json:"flash_model,omitempty"`      // pin what "flash" means instead of auto-picking
	FlashLiteModel string   `json:"flash_lite_model,omitempty"` // pin what "flash-lite" means
	ProModel       string   `json:"pro_model,omitempty"`        // pin what "pro" means
	ImageModel     string   `json:"image_model,omitempty"`      // pin what "image" means
	ImageProModel  string   `json:"image_pro_model,omitempty"`  // pin what "image-pro" means
	ImageDir       string   `json:"image_dir,omitempty"`        // where generated images go (default Pictures\Gemini)
	AllowedDirs    []string `json:"allowed_dirs,omitempty"`     // if set, only files inside these folders can be sent
}

func configDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "gemini-bridge"), nil
}

func configPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// loadConfigFile reads only the saved file (setup uses this so it never
// writes a key that came from an environment variable).
func loadConfigFile() (*Config, error) {
	cfg := &Config{}
	p, err := configPath()
	if err != nil {
		return cfg, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cfg, nil
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("the settings file %s isn't valid JSON (%v); fix or delete it, then run setup again", p, err)
	}
	return cfg, nil
}

// loadConfig is the file plus environment overrides.
func loadConfig() (*Config, error) {
	cfg, err := loadConfigFile()
	if err != nil {
		return nil, err
	}
	if k := firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY")); k != "" {
		cfg.APIKey = k
	}
	if m := os.Getenv("GEMINI_MODEL"); m != "" {
		cfg.DefaultModel = m
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	return cfg, nil
}

func saveConfig(cfg *Config) (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return p, os.WriteFile(p, append(b, '\n'), 0o600)
}

func defaultImageDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Pictures", "Gemini")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
