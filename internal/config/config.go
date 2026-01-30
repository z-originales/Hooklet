package config

import (
	"os"
	"strconv"
)

// Defaults
const (
	DefaultPort        = "8080"
	DefaultRabbitHost  = "localhost"
	DefaultRabbitPort  = "5672"
	DefaultRabbitUser  = "guest"
	DefaultRabbitPass  = "guest"
	DefaultSocketPath  = "./hooklet.sock"
	DefaultDBPath      = "./hooklet.db"
	DefaultMessageTTL  = 300000  // 5 minutes
	DefaultQueueExpiry = 3600000 // 1 hour
)

// Config holds the service configuration.
type Config struct {
	Port        string
	DBPath      string
	RabbitURL   string
	RabbitHost  string
	RabbitPort  string
	RabbitUser  string
	RabbitPass  string
	MessageTTL  int
	QueueExpiry int
	SocketPath  string
	AdminToken  string
}

// Load reads configuration from environment variables or uses defaults.
func Load() Config {
	return Config{
		Port:        getEnv("PORT", DefaultPort),
		DBPath:      getEnv("HOOKLET_DB_PATH", DefaultDBPath),
		RabbitURL:   os.Getenv("RABBITMQ_URL"),
		RabbitHost:  getEnv("RABBITMQ_HOST", DefaultRabbitHost),
		RabbitPort:  getEnv("RABBITMQ_PORT", DefaultRabbitPort),
		RabbitUser:  getEnv("RABBITMQ_USER", DefaultRabbitUser),
		RabbitPass:  getEnv("RABBITMQ_PASS", DefaultRabbitPass),
		MessageTTL:  getEnvInt("HOOKLET_MESSAGE_TTL", DefaultMessageTTL),
		QueueExpiry: getEnvInt("HOOKLET_QUEUE_EXPIRY", DefaultQueueExpiry),
		SocketPath:  getEnv("HOOKLET_SOCKET", DefaultSocketPath),
		AdminToken:  os.Getenv("HOOKLET_ADMIN_TOKEN"),
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
