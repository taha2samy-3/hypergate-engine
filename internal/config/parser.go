package config

import (
	"flag"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigPath = "/etc/hyper-engine/config.yaml"
	EnvConfigPath     = "ENGINE_CONFIG_PATH"
)

// LoadConfig discovers the config path, parses the configuration, and returns the path used and the config.
func LoadConfig() (*Config, string, error) {
	configPath := DefaultConfigPath

	// 1. Check Environment Variable Fallback
	if envPath := os.Getenv(EnvConfigPath); envPath != "" {
		configPath = envPath
	}

	// 2. CLI Parsing
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	var cliConfig, cliConfigShort string
	fs.StringVar(&cliConfig, "config", "", "Path to config file")
	fs.StringVar(&cliConfigShort, "c", "", "Path to config file (shorthand)")

	if err := fs.Parse(os.Args[1:]); err == nil {
		if cliConfigShort != "" {
			configPath = cliConfigShort
		}
		if cliConfig != "" {
			configPath = cliConfig
		}
	}

	cfg, err := ParseFile(configPath)
	if err != nil {
		return nil, configPath, err
	}

	return cfg, configPath, nil
}

// ParseFile reads, unmarshals, validates, and pre-compiles the YAML configuration from a given file.
func ParseFile(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", configPath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	if cfg.Version != "v1" {
		return nil, fmt.Errorf(`invalid config version %q, expected "v1"`, cfg.Version)
	}

	if cfg.Telemetry.Logging.Level == "" {
		cfg.Telemetry.Logging.Level = "INFO"
	}
	if cfg.Telemetry.Logging.Format == "" {
		cfg.Telemetry.Logging.Format = "console"
	}
	if cfg.Telemetry.Logging.OutputPath == "" {
		cfg.Telemetry.Logging.OutputPath = "stdout"
	}

	// 4. Pre-compile Regex Patterns (Crucial for Zero-Allocation Request Path)
	for i, route := range cfg.Router.Routes {
		for j, match := range route.Matches {
			if match.PathRegexPattern != "" {
				re, err := regexp.Compile(match.PathRegexPattern)
				if err != nil {
					return nil, fmt.Errorf("invalid path_regex_pattern at route %q index %d match index %d: %w", route.Name, i, j, err)
				}
				cfg.Router.Routes[i].Matches[j].CompiledPathRegex = re
			}

			for headerKey, headerMatch := range match.Headers {
				if headerMatch.RegexPattern != "" {
					re, err := regexp.Compile(headerMatch.RegexPattern)
					if err != nil {
						return nil, fmt.Errorf("invalid regex_pattern for header %q at route %q match %d: %w", headerKey, route.Name, j, err)
					}
					headerMatch.CompiledRegex = re
					cfg.Router.Routes[i].Matches[j].Headers[headerKey] = headerMatch
				}
			}
		}
	}

	return &cfg, nil
}
