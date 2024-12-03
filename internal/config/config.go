package config

import (
	"fmt"
	"os"
	"time"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Backend    BackendConfig    `yaml:"backend"`
	WSBackend  BackendConfig    `yaml:"ws_backend"`
	Blockchain BlockchainConfig `yaml:"blockchain"`
	Logging    LoggingConfig    `yaml:"logging"`
	Cache      CacheConfig      `yaml:"cache"`
}

type ServerConfig struct {
	ListenAddress string `yaml:"listenAddress" envconfig:"PROXY_LISTEN_ADDRESS"`
	ListenPort    uint   `yaml:"listenPort" envconfig:"PROXY_LISTEN_PORT"`
}

type BackendConfig struct {
	URLs []string `yaml:"urls" envconfig:"BACKEND_URLS"`
}

type BlockchainConfig struct {
	Confirmations int           `yaml:"confirmations" envconfig:"BLOCKCHAIN_CONFIRMATIONS"`
	GasFeeTTL     time.Duration `yaml:"gasFeeTTL" envconfig:"BLOCKCHAIN_GAS_FEE_TTL"`
}

type LoggingConfig struct {
	Level          string `yaml:"level" envconfig:"LOGGING_LEVEL"`
	LogDirectory   string `yaml:"logDirectory" envconfig:"LOGGING_DIRECTORY"`
	LogFileName    string `yaml:"logFileName" envconfig:"LOGGING_FILE_NAME"`
	MaxLogFileSize int    `yaml:"maxLogFileSize" envconfig:"LOGGING_MAX_FILE_SIZE"`
	MaxBackups     int    `yaml:"maxBackups" envconfig:"LOGGING_MAX_BACKUPS"`
	MaxAge         int    `yaml:"maxAge" envconfig:"LOGGING_MAX_AGE"`
}

type CacheConfig struct {
	Shards             int           `yaml:"shards" envconfig:"CACHE_SHARDS"`
	LifeWindow         time.Duration `yaml:"lifeWindow" envconfig:"CACHE_LIFE_WINDOW"`
	CleanWindow        time.Duration `yaml:"cleanWindow" envconfig:"CACHE_CLEAN_WINDOW"`
	MaxEntriesInWindow int           `yaml:"maxEntriesInWindow" envconfig:"CACHE_MAX_ENTRIES_IN_WINDOW"`
	MaxEntrySize       int           `yaml:"maxEntrySize" envconfig:"CACHE_MAX_ENTRY_SIZE"`
	Verbose            bool          `yaml:"verbose" envconfig:"CACHE_VERBOSE"`
	HardMaxCacheSize   int           `yaml:"hardMaxCacheSize" envconfig:"CACHE_HARD_MAX_CACHE_SIZE"`
}

// Singleton config instance with default values
var globalConfig = &Config{
	Server: ServerConfig{
		ListenAddress: "0.0.0.0",
		ListenPort:    8080,
	},
	Blockchain: BlockchainConfig{
		Confirmations: 15,
		GasFeeTTL:     10 * time.Second,
	},
	Backend: BackendConfig{
		URLs: []string{"https://mainnet.infura.io/v3/YOUR_INFURA_PROJECT_ID"},
	},
	WSBackend: BackendConfig{
		URLs: []string{"wss://mainnet.infura.io/ws/v3/YOUR_INFURA_PROJECT_ID"},
	},
	Logging: LoggingConfig{
		Level:          "info",
		LogDirectory:   "logs",
		LogFileName:    "app.log",
		MaxLogFileSize: 100,
		MaxBackups:     3,
		MaxAge:         28,
	},
	Cache: CacheConfig{
		Shards:             1024,
		LifeWindow:         10 * time.Minute,
		CleanWindow:        5 * time.Minute,
		MaxEntriesInWindow: 1000 * 10 * 60,
		MaxEntrySize:       500,
		Verbose:            false,
		HardMaxCacheSize:   8192,
	},
}

// Load initializes the configuration by reading from a YAML file and environment variables
func Load(configFile string) (*Config, error) {
	// Load config file as YAML if provided
	if configFile != "" {
		buf, err := os.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("error reading config file: %s", err)
		}
		err = yaml.Unmarshal(buf, globalConfig)
		if err != nil {
			return nil, fmt.Errorf("error parsing config file: %s", err)
		}
	}

	// Load config values from environment variables
	err := envconfig.Process("proxy", globalConfig)
	if err != nil {
		return nil, fmt.Errorf("error processing environment variables: %s", err)
	}

	// Validate required configurations
	if len(globalConfig.Backend.URLs) == 0 {
		return nil, fmt.Errorf("backend URLs must be provided")
	}

	return globalConfig, nil
}

// GetConfig returns the global config instance
func GetConfig() *Config {
	return globalConfig
}
