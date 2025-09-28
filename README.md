# SSE PubSub Application Setup Guide

## Features

✅ **SSE Notifications**: Real-time Server-Sent Events for instant message delivery  
✅ **GCP Pub/Sub Integration**: Dedicated queue per user with automatic subscription management  
✅ **Pluggable Storage**: Seamlessly switch between Redis, SQLite, or in-memory storage  
✅ **Firestore Authentication**: API key-based user authentication with hash verification  
✅ **Connection Management**: Prevents simultaneous SSE connections from the same user  
✅ **Rate Limiting**: API endpoint limited to 1 request per minute per user  
✅ **Auto-Reconnection**: Handles connection drops gracefully  

## Prerequisites

1. **GCP Project** with Pub/Sub and Firestore APIs enabled
2. **Service Account** with appropriate permissions:
   - Pub/Sub Admin (for creating topics/subscriptions)
   - Firestore User (for reading user documents)
3. **Redis Server** (if using Redis storage)

## Environment Variables

```bash
# Required
export GCP_PROJECT_ID="your-gcp-project-id"

# Optional
export STORAGE_TYPE="redis"  # Options: redis, sqlite, memory
export GOOGLE_APPLICATION_CREDENTIALS="/path/to/service-account.json"
export PORT="8080"
```

## Firestore Setup

Create a `users` collection in Firestore with documents structured like:

```json
{
  "id": "user123",
  "api_key_hash": "sha256_hash_of_api_key"
}
```

To generate an API key hash:
```go
import (
    "crypto/sha256"
    "encoding/hex"
)

hash := sha256.Sum256([]byte("your-api-key"))
apiKeyHash := hex.EncodeToString(hash[:])
```

## Installation & Running

```bash
# Initialize Go module
go mod init sse-pubsub-app
go mod tidy

# Install dependencies
go get cloud.google.com/go/firestore
go get cloud.google.com/go/pubsub  
go get github.com/gin-gonic/gin
go get github.com/go-redis/redis/v8
go get github.com/mattn/go-sqlite3
go get golang.org/x/time/rate
go get google.golang.org/api/option

# Run the application
go run main.go
```

## API Endpoints

### 1. SSE Endpoint
```bash
curl -H "X-API-Key: your-api-key" http://localhost:8080/api/sse
```
- **Purpose**: Establishes Server-Sent Events connection
- **Behavior**: 
  - Immediately sends latest cached message
  - Prevents multiple simultaneous connections per user  
  - Keeps connection alive with heartbeats

### 2. Latest Message API
```bash
curl -H "X-API-Key: your-api-key" http://localhost:8080/api/latest
```
- **Purpose**: Retrieves the most recent message
- **Rate Limit**: 1 request per minute per user
- **Response**: JSON with message content and timestamp

### 3. Health Check
```bash
curl http://localhost:8080/health
```
- **Purpose**: Service health monitoring
- **No authentication required**

## Storage Options

### Redis (Default)
```bash
export STORAGE_TYPE="redis"
# Requires Redis server running on localhost:6379
```

### SQLite
```bash
export STORAGE_TYPE="sqlite"  
# Creates cache.db file automatically
```

### In-Memory
```bash
export STORAGE_TYPE="memory"
# No persistence, data lost on restart
```

## Pub/Sub Topic/Subscription Naming Convention

- **Topic**: `user-{userID}-topic`
- **Subscription**: `user-{userID}-subscription`

The application automatically creates topics and subscriptions if they don't exist.

## Architecture Flow

1. **Startup**: Load user API key hashes from Firestore into memory
2. **Pub/Sub Setup**: Create subscriptions for all users and start listeners
3. **Message Handling**: 
   - Receive message from Pub/Sub
   - Store in cache (Redis/SQLite/Memory)
   - Forward to active SSE connection (if any)
4. **Client Connection**:
   - Authenticate via API key
   - Establish SSE connection
   - Receive latest cached message immediately
   - Get real-time updates as they arrive

## Testing with Sample Messages

Send a message to a user's Pub/Sub topic:

```bash
# Using gcloud CLI
gcloud pubsub topics publish user-user123-topic --message="Hello World"
```

The message will be:
1. Received by the application
2. Stored in the configured cache
3. Sent to the user's SSE connection (if active)
4. Available via the `/api/latest` endpoint

## Production Considerations

- **Load Balancing**: Use sticky sessions for SSE connections
- **Monitoring**: Add metrics for connection counts, message rates
- **Security**: Use HTTPS and validate API keys regularly  
- **Scaling**: Consider Redis Cluster for high availability
- **Error Handling**: Add exponential backoff for Pub/Sub reconnections

## Troubleshooting

- **"Invalid API key"**: Ensure API key hash matches Firestore document
- **"No message found"**: User hasn't received any Pub/Sub messages yet
- **SSE connection drops**: Check network stability and firewall settings
- **Rate limit exceeded**: Wait 1 minute between `/api/latest` requests