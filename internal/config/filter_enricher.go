package config

// RedisMetadataEnricherConfig holds the configuration for the Redis Metadata Enricher filter.
type RedisMetadataEnricherConfig struct {
	RedisService   string              `yaml:"redis_service"`
	CacheSizeMB    int                 `yaml:"cache_size_mb,omitempty"`
	CacheTimeout   string              `yaml:"cache_timeout,omitempty"`
	Variables      map[string]Variable `yaml:"variables"`
	KeyPattern     string              `yaml:"key_pattern"`
	OutputMappings []OutputMappingSpec `yaml:"output_mappings"`
}

// Variable defines how to extract and fall back a single input parameter.
type Variable struct {
	Source       string `yaml:"source"`
	Default      string `yaml:"default,omitempty"`
	RegexPattern string `yaml:"regex_pattern,omitempty"`
	JSONPath     string `yaml:"json_path,omitempty"`
}

// OutputMappingSpec defines where to extract from the returned JSON and which header to inject it into.
type OutputMappingSpec struct {
	JSONPath     string `yaml:"json_path,omitempty"`
	TargetHeader string `yaml:"target_header"`
}