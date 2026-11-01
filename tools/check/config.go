// Package check runs local image files against a clip-probe server and reports
// the nearest Immich asset for each. The clip-check CLI and the Clip Check
// desktop app are both thin shells around it.
package check

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config is shared by both tools through one file: DefaultPath, overlaid by
// CLIP_CHECK_* environment variables (CLIP_CHECK_TOKEN, CLIP_CHECK_MAX_DISTANCE …).
type Config struct {
	Server      string  `mapstructure:"server" json:"server"`
	Token       string  `mapstructure:"token" json:"token"`
	MaxDistance float64 `mapstructure:"max_distance" json:"maxDistance"`
	Limit       int     `mapstructure:"limit" json:"limit"`
	Workers     int     `mapstructure:"workers" json:"workers"`
	// Magick is the ImageMagick binary: empty finds it, "off" never uses it.
	Magick string `mapstructure:"magick" json:"magick"`
	// Optional, desktop only: where to fetch Immich thumbnails of the matches.
	// The key needs nothing beyond the asset.view permission.
	ImmichURL string `mapstructure:"immich_url" json:"immichUrl"`
	ImmichKey string `mapstructure:"immich_api_key" json:"immichApiKey"`
}

// Every key needs a default, or viper's Unmarshal ignores its env variable.
var defaults = map[string]any{
	"server":         "",
	"token":          "",
	"max_distance":   0.01, // clip-probe's own MAX_DISTANCE default
	"limit":          1,
	"workers":        4,
	"magick":         "",
	"immich_url":     "",
	"immich_api_key": "",
}

// DefaultPath is <UserConfigDir>/clip-check/config.yaml.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "clip-check.yaml"
	}
	return filepath.Join(dir, "clip-check", "config.yaml")
}

// Load reads file (DefaultPath when empty) with the environment on top. A
// missing file is not an error: everything can come from flags or env.
func Load(file string) (*viper.Viper, error) {
	if file == "" {
		file = DefaultPath()
	}
	v := viper.New()
	for k, d := range defaults {
		v.SetDefault(k, d)
	}
	v.SetEnvPrefix("CLIP_CHECK")
	v.AutomaticEnv()
	v.SetConfigFile(file)
	if err := v.ReadInConfig(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return v, nil
}

// Decode turns what Load (and any v.Set on top) gathered into a Config.
func Decode(v *viper.Viper) (Config, error) {
	var c Config
	err := v.Unmarshal(&c)
	return c, err
}

// Save writes c to file (DefaultPath when empty), creating its folder.
func Save(file string, c Config) error {
	if file == "" {
		file = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	v := viper.New()
	v.Set("server", c.Server)
	v.Set("token", c.Token)
	v.Set("max_distance", c.MaxDistance)
	v.Set("limit", c.Limit)
	v.Set("workers", c.Workers)
	v.Set("magick", c.Magick)
	v.Set("immich_url", c.ImmichURL)
	v.Set("immich_api_key", c.ImmichKey)
	return v.WriteConfigAs(file)
}

func (c Config) validate() error {
	switch {
	case c.Server == "":
		return errors.New("no server configured: set server in " + DefaultPath() + " or pass --server")
	case c.Token == "":
		return errors.New("no token configured: set token in " + DefaultPath() + " or CLIP_CHECK_TOKEN")
	}
	return nil
}
