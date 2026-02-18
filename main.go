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
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/pubsub"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/time/rate"
	"google.golang.org/api/iterator"
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
	ID                   string                         `firestore:"id"`
	Devices              map[string]Device              `firestore:"devices"`
	NotificationChannels map[string]NotificationChannel `firestore:"notificationChannels"`
}

type NotificationChannel struct {
	ID                 string `firestore:"id"`
	Type               string `firestore:"type"`
	APIKeyHash         string `firestore:"apiKeyHash"`
	Status             string `firestore:"status"`
	LastTokenGenerated string `firestore:"lastTokenGenerated,omitempty"` // omitempty for rest_1 only
}

type Device struct {
	ID               string  `firestore:"id" json:"id"`
	FriendlyName     string  `firestore:"friendlyName" json:"friendlyName"`
	RainDetected     bool    `firestore:"rainDetected" json:"rainDetected"`
	RainIntensityMmh float32 `firestore:"rainIntensityMmh" json:"rainIntensityMmh"`
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

func (rl *RateLimiter) GetLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.limiters[key]
	if !exists {
		limiter = rate.NewLimiter(rate.Every(time.Second), 1) // 1 request per second
		rl.limiters[key] = limiter
	}
	return limiter
}

// Application struct holds all dependencies
type Application struct {
	storage             Storage
	pubsubClient        *pubsub.Client
	firestoreClient     *firestore.Client
	sseConnections      map[string]*SSEConnection // userID -> connection
	connMutex           sync.RWMutex
	userIdRateLimiter   *RateLimiter
	clientIpRateLimiter *RateLimiter
}

func NewApplication() *Application {
	return &Application{
		sseConnections:      make(map[string]*SSEConnection),
		userIdRateLimiter:   NewRateLimiter(),
		clientIpRateLimiter: NewRateLimiter(),
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

// Initialize Firestore client
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
	query := app.firestoreClient.Collection("users").
		Where("notificationChannels.rest_1.status", "==", "active").
		Where("notificationChannels.rest_1.apiKeyHash", ">", "") // Checks for non-empty apiKeyHash

	iter := query.Documents(ctx)
	defer iter.Stop()

	i := 0

	for {
		doc, err := iter.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			log.Printf("Error iterating user documents to start Pub/Sub subscriptions: %v", err)
			return
		}
		userID := doc.Ref.ID
		go app.subscribePubSub(ctx, userID)
		i++
	}

	log.Printf("Started Pub/Sub subscriptions for %d users", i)
}

// Subscribe to Pub/Sub messages for a specific user
func (app *Application) subscribePubSub(ctx context.Context, userID string) {
	subscriptionName := fmt.Sprintf("%s-sub", userID)
	log.Printf("Ensuring Pub/Sub subscription %s exists", subscriptionName)
	subscription := app.pubsubClient.Subscription(subscriptionName)

	// Check if subscription exists, create if not
	exists, err := subscription.Exists(ctx)
	if err != nil {
		log.Printf("Error checking subscription existence for user %s: %v", userID, err)
		return
	}

	if !exists {
		log.Printf("Subscription %s does not exist for user %s, creating...", subscriptionName, userID)
		topicName := fmt.Sprintf("users_%s", userID) //prefix for this topic is already added by the client library
		topic := app.getOrCreateTopic(ctx, topicName)

		if topic == nil {
			log.Printf("Failed to get or create topic %s for user %s", topicName, userID)
			return
		}

		// Create subscription
		_, err = app.pubsubClient.CreateSubscription(ctx, subscriptionName, pubsub.SubscriptionConfig{
			Topic: topic,
		})
		if err != nil {
			log.Printf("Error creating subscription for user %s: %v", userID, err)
			return
		}

		log.Printf("Created Pub/Sub subscription %s for user %s", subscriptionName, userID)
	} else {
		log.Printf("Subscription %s already exists for user %s", subscriptionName, userID)
	}

	log.Printf("Starting to receive Pub/Sub messages for user %s on subscription %s", userID, subscriptionName)
	err = subscription.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		app.handlePubSubMessage(ctx, userID, string(msg.Data))
		msg.Ack()
	})

	if err != nil {
		log.Printf("Error receiving Pub/Sub messages for user %s: %v", userID, err)
	}
}

// Get or create Pub/Sub topic
func (app *Application) getOrCreateTopic(ctx context.Context, topicName string) *pubsub.Topic {
	topic := app.pubsubClient.Topic(topicName)
	exists, err := topic.Exists(ctx)
	if err != nil {
		log.Printf("Error checking topic existence: %v", err)
		return nil
	}

	if !exists {
		topic, err = app.pubsubClient.CreateTopic(ctx, topicName)
		if err != nil {
			log.Printf("Error creating topic %s: %v", topicName, err)
			return nil
		}
		log.Printf("Created Pub/Sub topic %s", topicName)
	}

	return topic
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

		parsedUserID, _, err := parseAPIKey(apiKey)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key format"})
			c.Abort()
			return
		}

		userDoc, err := app.firestoreClient.Collection("users").Doc(parsedUserID).Get(c.Request.Context())
		if err != nil {
			log.Printf("Error getting user document for %s: %v", parsedUserID, err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		var user User
		if err := userDoc.DataTo(&user); err != nil {
			log.Printf("Error unmarshalling user document for %s: %v", parsedUserID, err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		// Hash the actual key
		hash := sha256.Sum256([]byte(apiKey))
		actualKeyHash := hex.EncodeToString(hash[:])

		authenticated := false
		for _, channel := range user.NotificationChannels {
			if channel.Type == "restApi" && channel.APIKeyHash == actualKeyHash {
				authenticated = true
				break
			}
		}

		if !authenticated {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		// If user is authenticated and has an active rest_1 channel with an API key hash,
		// ensure their Pub/Sub subscription is active.
		restChannel, ok := user.NotificationChannels["rest_1"]
		if ok && restChannel.Status == "active" && restChannel.APIKeyHash != "" {
			go app.subscribePubSub(c.Request.Context(), parsedUserID)
		}

		c.Set("userID", parsedUserID)
		c.Next()
	}
}

// Middleware to rate limit API requests by IP address
func (app *Application) IpRateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		// Apply rate limiting
		limiter := app.clientIpRateLimiter.GetLimiter(ip)
		if !limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "Rate limit exceeded. Try again later.",
			})
			c.Abort()
			return
		}

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
	limiter := app.userIdRateLimiter.GetLimiter(userID)
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
			"error": "No recent data found for user",
		})
		return
	}

	// Try to parse as JSON, otherwise return as string
	var jsonData interface{}
	if err := json.Unmarshal([]byte(message), &jsonData); err != nil {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	c.JSON(http.StatusOK, jsonData)

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
	api.Use(app.IpRateLimitMiddleware())
	api.Use(app.AuthMiddleware())
	{
		api.GET("/sse", app.HandleSSE)
		api.GET("/latest", app.HandleGetLatestMessage)
	}

	// Start server
	log.Printf("Starting server on port %s with storage type: %s", port, storageType)

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

// parseAPIKey extracts the userID and actual key from the provided API key string.
// The format is "ug.<userID_base64_encoded>.<actualKey>".
func parseAPIKey(apiKey string) (string, string, error) {
	parts := strings.Split(apiKey, ".")
	if len(parts) != 3 || parts[0] != "ug" {
		return "", "", fmt.Errorf("invalid API key format")
	}

	userID := parts[1]
	actualKey := parts[2]

	return string(userID), actualKey, nil
}
