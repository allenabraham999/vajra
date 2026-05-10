package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the on-disk configuration written to ~/.vajra/config.json.
// Any field may be empty; the CLI degrades to API-key-only or JWT-only
// auth depending on what the user has provided.
type Config struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
	JWT    string `json:"jwt"`
	Email  string `json:"email,omitempty"`
}

// configPath returns the absolute path to the user's config file.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".vajra", "config.json"), nil
}

// loadConfig reads ~/.vajra/config.json. Returns a zero Config (no error)
// if the file doesn't exist — first-run users have nothing to load.
func loadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, nil
}

// saveConfig writes c to ~/.vajra/config.json with 0600 perms (file
// contains credentials). The directory is created on demand.
func saveConfig(c *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir config: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
