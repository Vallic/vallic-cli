// Package config holds what the CLI remembers between runs, and keeps the
// credential apart from the rest of it.
//
// Two files, deliberately. config.json is what people paste into an issue
// when something does not work; credentials.json is what must never appear
// there. One file holding both is one file somebody will share.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultAPI is the control plane a CLI with no configuration talks to.
const DefaultAPI = "https://console.vallic.com"

// Config is the non-secret half: where to talk, and what to assume.
type Config struct {
	// API is the control plane's base URL, without the /api/vc/v1 prefix.
	API string `json:"api,omitempty"`

	// Team is the team commands act in, as a machine name. With a personal
	// access token this is decoration — the token already narrows every
	// request to one team and nothing the CLI sends can widen it. With an
	// OAuth token it is the actual selection, because one login reaches
	// every team its owner belongs to.
	Team string `json:"team,omitempty"`

	// Project and Environment are only consulted when a checkout cannot
	// answer for itself. See internal/resolve.
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`

	// Format is the default output format when no --format is given.
	Format string `json:"format,omitempty"`

	// path is where this was read from, so Save writes it back.
	path string `json:"-"`
}

// Dir is the directory both files live in.
//
// XDG_CONFIG_HOME when the environment sets it, so a machine that puts
// configuration somewhere unusual is obeyed rather than second-guessed.
func Dir() (string, error) {
	if dir := os.Getenv("VALLIC_CONFIG_DIR"); dir != "" {
		return dir, nil
	}

	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "vallic"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a home directory to store configuration in: %w", err)
	}

	return filepath.Join(home, ".config", "vallic"), nil
}

// Load reads the configuration, or returns the defaults.
//
// A missing file is not an error: a CLI that has never been run has no
// configuration and that is its normal first state.
func Load() (*Config, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, "config.json")
	cfg := &Config{path: path}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg.applyEnv()
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	if err := json.Unmarshal(raw, cfg); err != nil {
		// Named, because the fix is to open that file and the message is the
		// only place its location appears.
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}

	cfg.path = path
	cfg.applyEnv()

	return cfg, nil
}

// applyEnv lets the environment win over the file.
//
// The order is the same one every command's target resolution uses: a flag
// beats the environment, the environment beats the file. CI sets variables
// and cannot edit a file in a container it did not build.
func (c *Config) applyEnv() {
	if v := os.Getenv("VALLIC_API"); v != "" {
		c.API = v
	}
	if v := os.Getenv("VALLIC_TEAM"); v != "" {
		c.Team = v
	}
	if v := os.Getenv("VALLIC_PROJECT"); v != "" {
		c.Project = v
	}
	if v := os.Getenv("VALLIC_ENVIRONMENT"); v != "" {
		c.Environment = v
	}

	if c.API == "" {
		c.API = DefaultAPI
	}

	c.API = strings.TrimRight(c.API, "/")
}

// Save writes the configuration back.
func (c *Config) Save() error {
	if c.path == "" {
		dir, err := Dir()
		if err != nil {
			return err
		}
		c.path = filepath.Join(dir, "config.json")
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(c.path), err)
	}

	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return writeAtomic(c.path, append(raw, '\n'), 0o600)
}

// writeAtomic writes beside the target and renames over it.
//
// The same beside-and-rename the agent uses for authorized_keys, for the
// same reason: a process killed halfway through a write leaves the old file
// intact rather than a truncated one, and a truncated credentials file is a
// logout nobody asked for.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("cannot write in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	// Before the content, so the secret is never briefly world-readable.
	// CreateTemp makes 0600 files, but saying so here is what keeps that
	// true if the permission argument ever means something else.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}
