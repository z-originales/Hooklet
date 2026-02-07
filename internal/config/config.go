package config

import (
	"os"
	"strconv"
	"strings"
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
}

// Load reads configuration from environment variables or uses defaults.
func Load() Config {
	return Config{
		Port:                         getEnv("PORT", DefaultPort),
		DBPath:                       getEnv("HOOKLET_DB_PATH", DefaultDBPath),
		RabbitURL:                    os.Getenv("RABBITMQ_URL"),
		RabbitHost:                   getEnv("RABBITMQ_HOST", DefaultRabbitHost),
		RabbitPort:                   getEnv("RABBITMQ_PORT", DefaultRabbitPort),
		RabbitUser:                   getEnv("RABBITMQ_USER", DefaultRabbitUser),
		RabbitPass:                   getEnv("RABBITMQ_PASS", DefaultRabbitPass),
		MessageTTL:                   getEnvInt("HOOKLET_MESSAGE_TTL", DefaultMessageTTL),
		QueueExpiry:                  getEnvInt("HOOKLET_QUEUE_EXPIRY", DefaultQueueExpiry),
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
		WSAllowedOrigins:             getEnvCSV("HOOKLET_WS_ORIGINS"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvCSV(key string) []string {
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
