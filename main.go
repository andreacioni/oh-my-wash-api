package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/pubsub"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
)

// Storage interface for pluggable cache mechanisms
type Storage interface {
	Set(ctx context.Context, key string, value string) error
	Get(ctx context.Context, key string) (string, error)
	Close() error
}

// RedisStorage implements Storage interface
type RedisStorage struct {
	client *redis.Client
}

func NewRedisStorage(addr, password string, db int) *RedisStorage {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	return &RedisStorage{client: rdb}
}

func (r *RedisStorage) Set(ctx context.Context, key string, value string) error {
	return r.client.Set(ctx, key, value, 0).Err()
}

func (r *RedisStorage) Get(ctx context.Context, key string) (string, error) {
	return r.client.Get(ctx, key).Result()
}

func (r *RedisStorage) Close() error {
	return r.client.Close()
}

// MemoryStorage implements Storage interface
type MemoryStorage struct {
	data map[string]string
	mu   sync.RWMutex
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		data: make(map[string]string),
	}
}

func (m *MemoryStorage) Set(ctx context.Context, key string, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *MemoryStorage) Get(ctx context.Context, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, exists := m.data[key]
	if !exists {
		return "", fmt.Errorf("key not found")
	}
	return value, nil
}

func (m *MemoryStorage) Close() error {
	return nil
}

// SQLiteStorage implements Storage interface
type SQLiteStorage struct {
	db *sql.DB
}

func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create table if not exists
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS cache (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return nil, err
	}

	return &SQLiteStorage{db: db}, nil
}

func (s *SQLiteStorage) Set(ctx context.Context, key string, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO cache (key, value, updated_at) VALUES (?, ?, ?)",
		key, value, time.Now())
	return err
}

func (s *SQLiteStorage) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM cache WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("key not found")
	}
	return value, err
}

func (s *SQLiteStorage) Close() error {
	return s.db.Close()
}

// User represents a user document from Firestore
type User struct {
	ID         string   `firestore:"id"`
	APIKeyHash string   `firestore:"apiKeyHash"`
	Devices    []Device `firestore:"devices"`
}

type Device struct {
	ID               string `firestore:"id"`
	RainDetected     string `firestore:"rainDetected"`
	RainIntensityMmh string `firestore:"rainIntensityMmh"`
}

// SSEConnection represents an active SSE connection
type SSEConnection struct {
	UserID string
	Writer gin.ResponseWriter
	Done   chan bool
}

// RateLimiter for API endpoints
type RateLimiter struct {
	limiters map[string]*rate.Limiter
	mu       sync.RWMutex
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		limiters: make(map[string]*rate.Limiter),
	}
}

func (rl *RateLimiter) GetLimiter(userID string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.limiters[userID]
	if !exists {
		limiter = rate.NewLimiter(rate.Every(time.Second), 1) // 1 request per second
		rl.limiters[userID] = limiter
	}
	return limiter
}

// Application struct holds all dependencies
type Application struct {
	storage         Storage
	pubsubClient    *pubsub.Client
	firestoreClient *firestore.Client
	userAPIKeys     map[string]string         // apiKeyHash -> userID
	sseConnections  map[string]*SSEConnection // userID -> connection
	connMutex       sync.RWMutex
	rateLimiter     *RateLimiter
}

func NewApplication() *Application {
	return &Application{
		userAPIKeys:    make(map[string]string),
		sseConnections: make(map[string]*SSEConnection),
		rateLimiter:    NewRateLimiter(),
	}
}

// Initialize storage based on configuration
func (app *Application) InitStorage(storageType string) error {
	switch storageType {
	case "redis":
		app.storage = NewRedisStorage("localhost:6379", "", 0)
	case "memory":
		app.storage = NewMemoryStorage()
	case "sqlite":
		storage, err := NewSQLiteStorage("./cache.db")
		if err != nil {
			return err
		}
		app.storage = storage
	default:
		return fmt.Errorf("unsupported storage type: %s", storageType)
	}
	return nil
}

// Initialize Firestore client and load user API keys
func (app *Application) InitFirestore(ctx context.Context, projectID, credentialsFile string) error {
	var client *firestore.Client
	var err error

	if credentialsFile != "" {
		client, err = firestore.NewClient(ctx, projectID, option.WithCredentialsFile(credentialsFile))
	} else {
		client, err = firestore.NewClient(ctx, projectID)
	}

	if err != nil {
		return fmt.Errorf("failed to create Firestore client: %v", err)
	}

	app.firestoreClient = client
	return app.loadUserAPIKeys(ctx)
}

// Load user API keys from Firestore
func (app *Application) loadUserAPIKeys(ctx context.Context) error {
	iter := app.firestoreClient.Collection("users").Documents(ctx)
	defer iter.Stop()

	for {
		doc, err := iter.Next()
		if err != nil {
			log.Printf("Error iterating user documents: %v", err)
			break
		}

		var user User
		if err := doc.DataTo(&user); err != nil {
			log.Printf("Error unmarshalling user document: %v", err)
			continue
		}

		app.userAPIKeys[user.APIKeyHash] = user.ID
	}

	log.Printf("Loaded %d user API keys", len(app.userAPIKeys))
	return nil
}

// Initialize Pub/Sub client
func (app *Application) InitPubSub(ctx context.Context, projectID, credentialsFile string) error {
	var client *pubsub.Client
	var err error

	if credentialsFile != "" {
		client, err = pubsub.NewClient(ctx, projectID, option.WithCredentialsFile(credentialsFile))
	} else {
		client, err = pubsub.NewClient(ctx, projectID)
	}

	if err != nil {
		return fmt.Errorf("failed to create Pub/Sub client: %v", err)
	}

	app.pubsubClient = client
	return nil
}

// Start Pub/Sub subscriptions for all users
func (app *Application) StartPubSubSubscriptions(ctx context.Context) {
	for _, userID := range app.userAPIKeys {
		go app.subscribePubSub(ctx, userID)
	}
}

// Subscribe to Pub/Sub messages for a specific user
func (app *Application) subscribePubSub(ctx context.Context, userID string) {
	subscriptionName := fmt.Sprintf("%s-sub", userID)

	subscription := app.pubsubClient.Subscription(subscriptionName)

	// Check if subscription exists, create if not
	exists, err := subscription.Exists(ctx)
	if err != nil {
		log.Printf("Error checking subscription existence for user %s: %v", userID, err)
		return
	}

	if !exists {
		topicName := fmt.Sprintf("projects/oh-my-wash-45358/topics/%s", userID)
		topic := app.pubsubClient.Topic(topicName)

		// Create subscription
		_, err = app.pubsubClient.CreateSubscription(ctx, subscriptionName, pubsub.SubscriptionConfig{
			Topic: topic,
		})
		if err != nil {
			log.Printf("Error creating subscription for user %s: %v", userID, err)
			return
		}
	}

	// Start receiving messages
	err = subscription.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		app.handlePubSubMessage(ctx, userID, string(msg.Data))
		msg.Ack()
	})

	if err != nil {
		log.Printf("Error receiving Pub/Sub messages for user %s: %v", userID, err)
	}
}

// Handle incoming Pub/Sub message
func (app *Application) handlePubSubMessage(ctx context.Context, userID, message string) {
	// Store message in cache
	key := fmt.Sprintf("user:%s:latest", userID)
	if err := app.storage.Set(ctx, key, message); err != nil {
		log.Printf("Error storing message for user %s: %v", userID, err)
		return
	}

	// Send message via SSE if user is connected
	app.connMutex.RLock()
	conn, exists := app.sseConnections[userID]
	app.connMutex.RUnlock()

	if exists {
		app.sendSSEMessage(conn, message)
	}
}

// Send SSE message to client
func (app *Application) sendSSEMessage(conn *SSEConnection, message string) {
	select {
	case <-conn.Done:
		return
	default:
		fmt.Fprintf(conn.Writer, "data: %s\n\n", message)
		conn.Writer.Flush()
	}
}

// Middleware to authenticate API key
func (app *Application) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-API-Key")
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
			c.Abort()
			return
		}

		// Hash the API key
		hash := sha256.Sum256([]byte(apiKey))
		apiKeyHash := hex.EncodeToString(hash[:])

		// Look up user ID
		userID, exists := app.userAPIKeys[apiKeyHash]
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		c.Set("userID", userID)
		c.Next()
	}
}

// SSE endpoint handler
func (app *Application) HandleSSE(c *gin.Context) {
	userID := c.GetString("userID")

	// Check if user already has an active SSE connection
	app.connMutex.Lock()

	if existingConn, exists := app.sseConnections[userID]; exists {
		// Close existing connection
		close(existingConn.Done)
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	// Create new connection
	conn := &SSEConnection{
		UserID: userID,
		Writer: c.Writer,
		Done:   make(chan bool),
	}
	app.sseConnections[userID] = conn
	app.connMutex.Unlock()

	// Send latest message immediately
	key := fmt.Sprintf("user:%s:latest", userID)
	if latestMessage, err := app.storage.Get(c.Request.Context(), key); err == nil {
		app.sendSSEMessage(conn, latestMessage)
	}

	// Keep connection alive
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-conn.Done:
			return
		case <-c.Request.Context().Done():
			app.connMutex.Lock()
			close(conn.Done)
			delete(app.sseConnections, userID)
			app.connMutex.Unlock()
			return
		case <-ticker.C:
			// Send keepalive
			fmt.Fprintf(conn.Writer, ": keepalive\n\n")
			conn.Writer.Flush()
		}
	}
}

// API endpoint to get latest message
func (app *Application) HandleGetLatestMessage(c *gin.Context) {
	userID := c.GetString("userID")

	// Apply rate limiting
	limiter := app.rateLimiter.GetLimiter(userID)
	if !limiter.Allow() {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Rate limit exceeded. Try again later.",
		})
		return
	}

	// Get latest message from storage
	key := fmt.Sprintf("user:%s:latest", userID)
	message, err := app.storage.Get(c.Request.Context(), key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "No message found for user",
		})
		return
	}

	// Try to parse as JSON, otherwise return as string
	var jsonData interface{}
	if err := json.Unmarshal([]byte(message), &jsonData); err == nil {
		c.JSON(http.StatusOK, gin.H{
			"message":   jsonData,
			"timestamp": time.Now().Unix(),
		})
	} else {
		c.JSON(http.StatusOK, gin.H{
			"message":   message,
			"timestamp": time.Now().Unix(),
		})
	}
}

// Health check endpoint
func (app *Application) HandleHealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
	})
}

func main() {
	// Initialize application
	app := NewApplication()

	// Get configuration from environment variables
	storageType := getEnv("STORAGE_TYPE", "memory")
	projectID := getEnv("GCP_PROJECT_ID", "")
	credentialsFile := getEnv("GOOGLE_APPLICATION_CREDENTIALS", "")
	port := getEnv("PORT", "8080")

	if projectID == "" {
		log.Fatal("GCP_PROJECT_ID environment variable is required")
	}

	ctx := context.Background()

	// Initialize storage
	if err := app.InitStorage(storageType); err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer app.storage.Close()

	// Initialize Firestore
	if err := app.InitFirestore(ctx, projectID, credentialsFile); err != nil {
		log.Fatalf("Failed to initialize Firestore: %v", err)
	}
	defer app.firestoreClient.Close()

	// Initialize Pub/Sub
	if err := app.InitPubSub(ctx, projectID, credentialsFile); err != nil {
		log.Fatalf("Failed to initialize Pub/Sub: %v", err)
	}
	defer app.pubsubClient.Close()

	// Start Pub/Sub subscriptions
	app.StartPubSubSubscriptions(ctx)

	// Setup Gin router
	r := gin.Default()

	// Health check endpoint (no auth required)
	r.GET("/health", app.HandleHealthCheck)

	// Protected endpoints
	api := r.Group("/api")
	api.Use(app.AuthMiddleware())
	{
		api.GET("/sse", app.HandleSSE)
		api.GET("/latest", app.HandleGetLatestMessage)
	}

	// Start server
	log.Printf("Starting server on port %s with storage type: %s", port, storageType)
	log.Printf("Loaded %d users", len(app.userAPIKeys))

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// Helper function to get environment variables with defaults
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
