package master

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr         string
	RegistryURL        string
	AgentPort          int
	AgentTaskPath      string
	SharedToken        string
	MaxParallel        int
	RegistryTimeout    time.Duration
	TaskDefaultTimeout time.Duration
	TaskMaxTimeout     time.Duration
	AllowedOrigin      string
}

func LoadConfig() (Config, error) {
	cfg := Config{
		ListenAddr:         envOrDefault("MASTER_LISTEN_ADDR", ":8001"),
		RegistryURL:        envOrDefault("NODE_REGISTRY_URL", "http://127.0.0.1:8888/api/nodes"),
		AgentPort:          8002,
		AgentTaskPath:      envOrDefault("AGENT_TASK_PATH", "/api/v1/tasks"),
		SharedToken:        strings.TrimSpace(os.Getenv("AGENT_SHARED_TOKEN")),
		MaxParallel:        20,
		RegistryTimeout:    3 * time.Second,
		TaskDefaultTimeout: 10 * time.Second,
		TaskMaxTimeout:     30 * time.Second,
		AllowedOrigin:      envOrDefault("CORS_ALLOWED_ORIGIN", "*"),
	}

	var err error
	if cfg.AgentPort, err = intEnv("AGENT_PORT", cfg.AgentPort, 1, 65535); err != nil {
		return Config{}, err
	}
	if cfg.MaxParallel, err = intEnv("MASTER_MAX_PARALLEL", cfg.MaxParallel, 1, 500); err != nil {
		return Config{}, err
	}
	if cfg.RegistryTimeout, err = durationEnv("REGISTRY_TIMEOUT", cfg.RegistryTimeout); err != nil {
		return Config{}, err
	}
	if cfg.TaskDefaultTimeout, err = durationEnv("TASK_DEFAULT_TIMEOUT", cfg.TaskDefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.TaskMaxTimeout, err = durationEnv("TASK_MAX_TIMEOUT", cfg.TaskMaxTimeout); err != nil {
		return Config{}, err
	}
	if cfg.TaskDefaultTimeout > cfg.TaskMaxTimeout {
		return Config{}, fmt.Errorf("TASK_DEFAULT_TIMEOUT cannot exceed TASK_MAX_TIMEOUT")
	}
	parsedRegistryURL, err := url.ParseRequestURI(cfg.RegistryURL)
	if err != nil || parsedRegistryURL.Host == "" || (parsedRegistryURL.Scheme != "http" && parsedRegistryURL.Scheme != "https") {
		return Config{}, fmt.Errorf("NODE_REGISTRY_URL must be a valid HTTP(S) URL")
	}
	if !strings.HasPrefix(cfg.AgentTaskPath, "/") {
		return Config{}, fmt.Errorf("AGENT_TASK_PATH must start with /")
	}
	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func intEnv(key string, fallback, minimum, maximum int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", key, minimum, maximum)
	}
	return parsed, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}
