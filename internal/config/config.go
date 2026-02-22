package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"
)

// Defaults
const (
	DefaultPort                         = "8080"
	DefaultRabbitHost                   = "localhost"
	DefaultRabbitPort                   = "5672"
	DefaultRabbitUser                   = "guest"
	DefaultRabbitPass                   = "guest"
	DefaultSocketPath                   = "./hooklet.sock"
	DefaultDBPath                       = "./hooklet.db"
	DefaultMessageTTL                   = 300000  // 5 minutes
	DefaultQueueExpiry                  = 3600000 // 1 hour
	DefaultMaxBodyBytes                 = 1048576 // 1 MB
	DefaultWSAuthMaxBytes               = 8192    // 8 KB
	DefaultHTTPReadTimeoutSeconds       = 15
	DefaultHTTPWriteTimeoutSeconds      = 15
	DefaultHTTPIdleTimeoutSeconds       = 60
	DefaultHTTPReadHeaderTimeoutSeconds = 10
	DefaultWSWriteTimeoutSeconds        = 10
	DefaultWSAuthTimeoutSeconds         = 10
	DefaultLogLevel                     = "info"
)

// Config holds the service configuration.
type Config struct {
	Port                         string
	DBPath                       string
	RabbitURL                    string
	RabbitHost                   string
	RabbitPort                   string
	RabbitUser                   string
	RabbitPass                   string
	MessageTTL                   int
	QueueExpiry                  int
	SocketPath                   string
	AdminToken                   string
	AdminDebug                   bool
	MaxBodyBytes                 int64
	WSAuthMaxBytes               int64
	HTTPReadTimeoutSeconds       int
	HTTPWriteTimeoutSeconds      int
	HTTPIdleTimeoutSeconds       int
	HTTPReadHeaderTimeoutSeconds int
	WSWriteTimeoutSeconds        int
	WSAuthTimeoutSeconds         int
	WSAllowedOrigins             []string
	LogLevel                     string
}

// Load reads configuration from environment variables or uses defaults.
func Load() Config {
	cfg := Config{
		Port:                         getEnv("PORT", DefaultPort),
		DBPath:                       getEnv("HOOKLET_DB_PATH", DefaultDBPath),
		RabbitURL:                    os.Getenv("RABBITMQ_URL"),
		RabbitHost:                   getEnv("RABBITMQ_HOST", DefaultRabbitHost),
		RabbitPort:                   getEnv("RABBITMQ_PORT", DefaultRabbitPort),
		RabbitUser:                   getEnv("RABBITMQ_USER", DefaultRabbitUser),
		RabbitPass:                   getEnv("RABBITMQ_PASS", DefaultRabbitPass),
		MessageTTL:                   getEnvInt("HOOKLET_MESSAGE_TTL", DefaultMessageTTL),   // TODO : Error if lt 0 and disable if eq 0
		QueueExpiry:                  getEnvInt("HOOKLET_QUEUE_EXPIRY", DefaultQueueExpiry), //TODO : warning if eq MessageTTL and error if lt MessageTTL and disable if eq 0
		SocketPath:                   getEnv("HOOKLET_SOCKET", DefaultSocketPath),
		AdminToken:                   os.Getenv("HOOKLET_ADMIN_TOKEN"),
		AdminDebug:                   getEnvBool("HOOKLET_ADMIN_DEBUG", false),
		MaxBodyBytes:                 int64(getEnvInt("HOOKLET_MAX_BODY_BYTES", DefaultMaxBodyBytes)),
		WSAuthMaxBytes:               int64(getEnvInt("HOOKLET_WS_AUTH_MAX_BYTES", DefaultWSAuthMaxBytes)),
		HTTPReadTimeoutSeconds:       getEnvInt("HOOKLET_HTTP_READ_TIMEOUT", DefaultHTTPReadTimeoutSeconds),
		HTTPWriteTimeoutSeconds:      getEnvInt("HOOKLET_HTTP_WRITE_TIMEOUT", DefaultHTTPWriteTimeoutSeconds),
		HTTPIdleTimeoutSeconds:       getEnvInt("HOOKLET_HTTP_IDLE_TIMEOUT", DefaultHTTPIdleTimeoutSeconds),
		HTTPReadHeaderTimeoutSeconds: getEnvInt("HOOKLET_HTTP_READ_HEADER_TIMEOUT", DefaultHTTPReadHeaderTimeoutSeconds),
		WSWriteTimeoutSeconds:        getEnvInt("HOOKLET_WS_WRITE_TIMEOUT", DefaultWSWriteTimeoutSeconds),
		WSAuthTimeoutSeconds:         getEnvInt("HOOKLET_WS_AUTH_TIMEOUT", DefaultWSAuthTimeoutSeconds),
		WSAllowedOrigins:             getEnvComma("HOOKLET_WS_ORIGINS"),
		LogLevel:                     getEnv("HOOKLET_LOG_LEVEL", DefaultLogLevel),
	}

	// Validate configuration
	if cfg.Port == "" {
		cfg.Port = DefaultPort
	}
	if cfg.MessageTTL < 0 {
		log.Warn("Invalid HOOKLET_MESSAGE_TTL (must be >= 0), using default", "value", cfg.MessageTTL)
		cfg.MessageTTL = DefaultMessageTTL
	}
	if cfg.QueueExpiry < 0 {
		log.Warn("Invalid HOOKLET_QUEUE_EXPIRY (must be >= 0), using default", "value", cfg.QueueExpiry)
		cfg.QueueExpiry = DefaultQueueExpiry
	}
	if cfg.MaxBodyBytes <= 0 {
		log.Warn("Invalid HOOKLET_MAX_BODY_BYTES (must be > 0), using default", "value", cfg.MaxBodyBytes)
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}

	return cfg
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		log.Warn("Invalid integer for env var, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return i
}

func getEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Warn("Invalid boolean for env var, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return b
}

func getEnvComma(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
