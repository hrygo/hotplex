# HotPlex Java Client

> Java client SDK for HotPlex Gateway

[![Maven Central](https://img.shields.io/maven-central/v/dev.hotplex/hotplex-client.svg)](https://mavenbadges.herokuapp.com/maven-central/dev.hotplex/hotplex-client)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

---

## Status

🚧 **Under Development** - API may change

---

## Requirements

- Java 17+
- Maven 3.8+ or Gradle 8+

---

## Installation

Build this reference SDK from source with `mvn clean install` before using the
coordinates below. This installs `1.52.0-SNAPSHOT` in your local Maven repository;
the Gateway release does not publish SDK packages to Maven Central.

### Maven

```xml
<dependency>
    <groupId>dev.hotplex</groupId>
    <artifactId>hotplex-client</artifactId>
    <version>1.52.0-SNAPSHOT</version>
</dependency>
```

### Gradle (Groovy)

```groovy
implementation 'dev.hotplex:hotplex-client:1.52.0-SNAPSHOT'
```

### Gradle (Kotlin)

```kotlin
implementation("dev.hotplex:hotplex-client:1.52.0-SNAPSHOT")
```

---

## Quick Start

```java
import dev.hotplex.client.*;
import dev.hotplex.protocol.*;

public class Main {
    public static void main(String[] args) throws Exception {
        // Create client
        HotPlexClient client = HotPlexClient.builder()
            .url("ws://localhost:8888")
            .workerType(WorkerType.CLAUDE_CODE)
            .apiKey("your-api-key")
            .build();

        // Register event listeners
        client.onMessageDelta(data -> {
            System.out.print(data.getContent());
        });

        client.onDone(data -> {
            System.out.printf("%n✅ Done! Success: %b%n", data.isSuccess());
            if (data.getStats() != null) {
                System.out.printf("   Duration: %dms%n", data.getStats().getDurationMs());
                System.out.printf("   Tokens: %d%n", data.getStats().getTotalTokens());
            }
        });

        client.onError(data -> {
            System.err.printf("Error [%s]: %s%n", data.getCode(), data.getMessage());
        });

        // Connect
        try {
            client.connect();
            System.out.println("Connected! Session: " + client.getSessionId());

            // Send input
            client.sendInput(InputData.builder()
                .content("Write a hello world in Java")
                .build());

            // Wait for completion
            Thread.sleep(30000);
        } finally {
            client.close();
        }
    }
}
```

---

## API Reference

### Builder

```java
HotPlexClient client = HotPlexClient.builder()
    .url("ws://localhost:8888")                    // Required
    .workerType(WorkerType.CLAUDE_CODE)            // Required
    .apiKey("your-api-key")                        // Optional
    .botId("bot-123")                              // Optional (multi-bot isolation)
    .sessionId("existing-session-id")              // Optional (resume session)
    .reconnect(true)                               // Optional (default: true)
    .reconnectMaxAttempts(5)                       // Optional (default: 5)
    .timeout(Duration.ofSeconds(30))               // Optional (default: 30s)
    .metadata(Map.of("key", "value"))              // Optional
    .build();
```

### Connection Methods

```java
// Connect establishes WebSocket connection and initializes session
client.connect();

// Close closes connection and cleanup resources
client.close();

// GetSessionId returns current session ID
String sessionId = client.getSessionId();

// IsConnected returns connection status
boolean connected = client.isConnected();
```

### Sending Messages

#### Input

```java
client.sendInput(InputData.builder()
    .content("Your input here")
    .metadata(Map.of("language", "java"))
    .build());
```

#### Tool Result

```java
client.sendToolResult(ToolResultData.builder()
    .toolCallId("call_123")
    .output("result")
    .error(null) // optional
    .build());
```

#### Permission Response

```java
client.sendPermissionResponse(PermissionResponseData.builder()
    .permissionId("perm_456")
    .allowed(true)
    .reason("User approved")
    .build());
```

---

## Event Listeners

### Message Events

```java
// Message start
client.onMessageStart(data -> {
    System.out.println("Message started: " + data.getId());
});

// Message delta (streaming)
client.onMessageDelta(data -> {
    System.out.print(data.getContent());
});

// Message end
client.onMessageEnd(data -> {
    System.out.println("Message ended: " + data.getId());
});
```

### Tool Events

```java
client.onToolCall(data -> {
    // Execute tool
    String result = executeTool(data.getName(), data.getInput());

    // Send result back
    client.sendToolResult(ToolResultData.builder()
        .toolCallId(data.getId())
        .output(result)
        .build());
});
```

### Permission Events

```java
client.onPermissionRequest(data -> {
    boolean allowed = askUser(data.getToolName(), data.getDescription());

    client.sendPermissionResponse(PermissionResponseData.builder()
        .permissionId(data.getId())
        .allowed(allowed)
        .reason(allowed ? "User approved" : "User denied")
        .build());
});
```

### Lifecycle Events

```java
// State change
client.onState(data -> {
    System.out.println("State: " + data.getState());

    if (data.getState() == SessionState.IDLE) {
        System.out.println("Worker idle, ready for input");
    }
});

// Completion
client.onDone(data -> {
    System.out.println("Done! Success: " + data.isSuccess());
});

// Error
client.onError(data -> {
    System.err.println("Error: " + data.getCode() + " - " + data.getMessage());
});
```

### Connection Events

```java
client.onConnected(() -> {
    System.out.println("WebSocket connected");
});

client.onDisconnected(() -> {
    System.out.println("WebSocket disconnected");
});

client.onReconnecting((attempt, maxAttempts) -> {
    System.out.printf("Reconnecting %d/%d%n", attempt, maxAttempts);
});
```

---

## Data Classes

### InputData

```java
InputData data = InputData.builder()
    .content("Your input")
    .metadata(Map.of("key", "value"))
    .build();
```

### MessageDeltaData

```java
String content = data.getContent();
```

### ToolCallData

```java
String id = data.getId();
String name = data.getName();
Map<String, Object> input = data.getInput();
```

### DoneData

```java
boolean success = data.isSuccess();
DoneStats stats = data.getStats();

if (stats != null) {
    int durationMs = stats.getDurationMs();
    int totalTokens = stats.getTotalTokens();
    double costUsd = stats.getCostUsd();
}
```

### ErrorData

```java
String code = data.getCode();
String message = data.getMessage();
Map<String, Object> details = data.getDetails();
```

---

## Advanced Usage

### Session Resumption

```java
// First session
HotPlexClient client1 = HotPlexClient.builder()
    .url("ws://localhost:8888")
    .workerType(WorkerType.CLAUDE_CODE)
    .apiKey("your-api-key")
    .build();

client1.connect();
String sessionId = client1.getSessionId();
client1.sendInput(InputData.builder()
    .content("Start task...")
    .build());

// Later: resume session
HotPlexClient client2 = HotPlexClient.builder()
    .url("ws://localhost:8888")
    .workerType(WorkerType.CLAUDE_CODE)
    .apiKey("your-api-key")
    .sessionId(sessionId) // Resume
    .build();

client2.connect();
client2.sendInput(InputData.builder()
    .content("Continue task...")
    .build());
```

### Async Pattern with CompletableFuture

```java
import java.util.concurrent.CompletableFuture;

public CompletableFuture<DoneData> sendInputAsync(String content) {
    return client.sendInputAsync(content);
}

// Usage
sendInputAsync("Write hello world")
    .thenAccept(data -> {
        System.out.println("Task completed! Success: " + data.isSuccess());
    })
    .exceptionally(err -> {
        System.err.println("Task failed: " + err.getMessage());
        return null;
    });
```

### Tool Implementation

```java
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Paths;

client.onToolCall(data -> {
    try {
        String output;

        switch (data.getName()) {
            case "bash":
                output = executeBash(data.getInput().get("command").toString());
                break;

            case "read_file":
                String path = data.getInput().get("path").toString();
                output = Files.readString(Paths.get(path));
                break;

            default:
                throw new IllegalArgumentException("Unknown tool: " + data.getName());
        }

        client.sendToolResult(ToolResultData.builder()
            .toolCallId(data.getId())
            .output(output)
            .build());

    } catch (Exception err) {
        client.sendToolResult(ToolResultData.builder()
            .toolCallId(data.getId())
            .output("")
            .error(err.getMessage())
            .build());
    }
});
```

### Graceful Shutdown

```java
Runtime.getRuntime().addShutdownHook(new Thread(() -> {
    System.out.println("\nShutting down...");
    try {
        client.close();
        System.out.println("Client closed");
    } catch (Exception err) {
        System.err.println("Error during shutdown: " + err.getMessage());
    }
}));
```

---

## Error Handling

### Exception Hierarchy

```
HotPlexException (base)
├── ConnectionException
├── SessionException
└── TimeoutException
```

### Handling Errors

```java
import dev.hotplex.client.exception.*;

try {
    client.connect();
} catch (ConnectionException err) {
    System.err.println("Connection failed: " + err.getMessage());
} catch (SessionException err) {
    System.err.println("Session error: " + err.getMessage());
} catch (HotPlexException err) {
    System.err.println("HotPlex error: " + err.getMessage());
}
```

### Common Error Codes

| Code | Meaning | Action |
|------|---------|--------|
| `SESSION_NOT_FOUND` | Session doesn't exist | Create new session |
| `SESSION_TERMINATED` | Session terminated | Create new session |
| `UNAUTHORIZED` | Invalid API key | Check API key |
| `INVALID_INPUT` | Malformed input | Check message format |

---

## Testing

### Unit Tests

```bash
mvn test
```

### Integration Tests

```bash
# Start gateway
./hotplex &

# Run integration tests
mvn verify -Pintegration-test
```

### Test Example

```java
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;

class ClientTest {
    @Test
    void testConnection() throws Exception {
        HotPlexClient client = HotPlexClient.builder()
            .url("ws://localhost:8888")
            .workerType(WorkerType.CLAUDE_CODE)
            .apiKey("test-key")
            .build();

        assertDoesNotThrow(() -> client.connect());
        assertNotNull(client.getSessionId());

        client.close();
    }
}
```

---

## Architecture

```
┌─────────────────────────────────────────┐
│         HotPlexClient                   │
│  - Event listeners (on*)                │
│  - Message builders (send*)             │
│  - State management                     │
├─────────────────────────────────────────┤
│         Transport (WebSocket)           │
│  - Connection lifecycle                 │
│  - Auto-reconnect with backoff          │
│  - Message queue                        │
├─────────────────────────────────────────┤
│         Protocol (AEP v1)               │
│  - NDJSON codec                         │
│  - Envelope builder                     │
│  - Event type definitions               │
└─────────────────────────────────────────┘
```

**Package Structure**:
```
dev.hotplex.client         # High-level client API
dev.hotplex.protocol       # AEP codec and types
dev.hotplex.exception      # Exception hierarchy
```

---

## Development

### Makefile Commands

A `Makefile` is provided for convenience:

```bash
make help        # Show help
make build       # Compile the project
make test        # Run tests
make run         # Run QuickStart example
make interactive # Run Interactive example
make install     # Install to local Maven repo
```

### Build

```bash
mvn clean package
```

### Test

```bash
mvn test
```

### Install Locally

```bash
mvn clean install
```

---

## License

Apache-2.0

---

## Related

- **Protocol Spec**: `docs/architecture/AEP-v1-Protocol.md`
- **Python Client**: `examples/python-client/`
- **TypeScript Client**: `examples/typescript-client/`
- **Go Client**: `client/`
