package config

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	DefaultConfigPath = "/etc/hyper-engine/config.yaml"
	EnvConfigPath     = "CONFIG_FILE_PATH"
	EnvConfigProvider = "CONFIG_PROVIDER"
	EnvConfigURL      = "CONFIG_URL"
)

func LoadConfig() (*Config, string, error) {
	provider := os.Getenv(EnvConfigProvider)
	if provider == "" {
		provider = "FILE"
	}

	var data []byte
	var err error
	configPath := ""

	if provider == "K8S" {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, "", fmt.Errorf("failed to load in-cluster config: %w", err)
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create kubernetes client: %w", err)
		}

		cmName := os.Getenv("CONFIG_K8S_NAME")
		if cmName == "" {
			cmName = "hyper-engine-config"
		}
		namespace := os.Getenv("CONFIG_K8S_NAMESPACE")
		if namespace == "" {
			namespace = "hyper-system"
		}

		cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), cmName, metav1.GetOptions{})
		if err != nil {
			return nil, "", fmt.Errorf("failed to get configmap %s: %w", cmName, err)
		}

		yamlContent, ok := cm.Data["config.yaml"]
		if !ok {
			return nil, "", fmt.Errorf("config.yaml not found in configmap %s", cmName)
		}
		data = []byte(yamlContent)
		configPath = cmName
	} else if provider == "URL" {
		configURL := os.Getenv(EnvConfigURL)
		if configURL == "" {
			return nil, "", fmt.Errorf("config provider set to URL but CONFIG_URL is empty")
		}
		configPath = configURL

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(configURL)
		if err != nil {
			return nil, "", fmt.Errorf("failed to fetch remote config: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("remote config server returned non-200 status: %d", resp.StatusCode)
		}

		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read remote config body: %w", err)
		}
	} else {
		configPath = DefaultConfigPath
		if envPath := os.Getenv(EnvConfigPath); envPath != "" {
			configPath = envPath
		}

		fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		var cliConfig, cliConfigShort string
		fs.StringVar(&cliConfig, "config", "", "Path to config file")
		fs.StringVar(&cliConfigShort, "c", "", "Path to config file")
		_ = fs.Parse(os.Args[1:])

		if cliConfig != "" {
			configPath = cliConfig
		} else if cliConfigShort != "" {
			configPath = cliConfigShort
		}

		data, err = os.ReadFile(configPath)
		if err != nil {
			return nil, configPath, fmt.Errorf("failed to read config file %q: %w", configPath, err)
		}
	}

	cfg, err := ParseBytes(data)
	if err != nil {
		return nil, configPath, err
	}

	return cfg, configPath, nil
}

func ParseBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	if cfg.Version != "v1" {
		return nil, fmt.Errorf(`invalid config version %q, expected "v1"`, cfg.Version)
	}

	if cfg.Server.PoolPrewarmSize <= 0 {
		cfg.Server.PoolPrewarmSize = 5000
	}
	if cfg.Server.InitialHeaderCapacity <= 0 {
		cfg.Server.InitialHeaderCapacity = 64
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
