# Oh My Wash! API

This is the official API for the "Oh My Wash!" application. This API is designed to be used by users who have activated the "REST API notification" feature within the application and have obtained an API Key.

## Architecture

The data provided by this API originates from **Google Cloud Pub/Sub**. When one of your "Oh My Wash!" devices changes its status (e.g., detects rain), it publishes a message to a user-specific Pub/Sub topic. This API listens for new messages on that topic. Upon receiving a message, it caches the latest device information, making it available for you to query.

## API Integration

To integrate with the API, you must include your unique API Key in the `X-API-Key` HTTP header for every request.

There are two primary endpoints available for you to use:

### 1. Get Latest Device Status (REST)

This endpoint allows you to fetch the most recent status of all your registered devices at once.

*   **Endpoint:** `GET /api/latest`
*   **Input:** Your API key must be provided in the `X-API-Key` header.
*   **Output Example (`200 OK`):** A JSON object containing your devices, indexed by their unique ID.

    ```json
    {
      "device_id_1": {
        "deviceId": "device_id_1",
        "friendlyName": "Garden Sprinkler",
        "rainDetected": true,
        "rainIntensityMmh": 1.2
      },
      "device_id_2": {
        "deviceId": "device_id_2",
        "friendlyName": "Front Yard",
        "rainDetected": false,
        "rainIntensityMmh": 0
      }
    }
    ```

### 2. Real-Time Updates (Server-Sent Events)

This endpoint provides a persistent connection that streams updates to you in real-time as they happen. This is the most efficient way to get immediate notifications.

*   **Endpoint:** `GET /api/sse`
*   **Input:** Your API key must be provided in the `X-API-Key` header.
*   **Output Example:** A stream of `text/event-stream` data. Upon connection, the first event provides the full current state of all registered devices. Subsequent events are sent as individual device updates occur.

    ```
    data: {"device_id_1": {"deviceId": "device_id_1", "friendlyName": "Garden Sprinkler", "rainDetected": true, "rainIntensityMmh": 2.5}, "device_id_2": {"deviceId": "device_id_2", "friendlyName": "Front Yard", "rainDetected": false, "rainIntensityMmh": 0}}

    : keepalive

    data: {"deviceId": "device_id_1", "friendlyName": "Garden Sprinkler", "rainDetected": false, "rainIntensityMmh": 0}
    ```

## Limitations

To ensure the stability and fair use of the service, the API enforces a rate limit. Each user is permitted to make **1 request per second** to the `/api/latest` endpoint. If you exceed this limit, you will receive a `429 Too Many Requests` error.

There is no rate limit on the `/api/sse` endpoint, but only one active connection is allowed per user.

## Build Instructions

To build and run this project from the source code, you will need to have Go installed.

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/your-username/oh-my-wash-api.git
    cd oh-my-wash-api
    ```

2.  **Install dependencies:**
    This command will download the necessary Go modules.
    ```bash
    go mod tidy
    ```

3.  **Build the application:**
    This will compile the source code into a single executable file.
    ```bash
    go build -o oh-my-wash-api main.go
    ```

4.  **Run the application:**
    Before running, you must configure the required environment variables.
    ```bash
    export STORAGE_TYPE=memory
    export GCP_PROJECT_ID=<your-gcp-project-id>
    export GOOGLE_APPLICATION_CREDENTIALS=/path/to/your/credentials.json
    export PORT=8080 # This is optional and defaults to 8080

    ./oh-my-wash-api
    ```
