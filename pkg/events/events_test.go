package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionState_IsTerminal(t *testing.T) {
	tests := []struct {
		name     string
		state    SessionState
		terminal bool
	}{
		{
			name:     "created is not terminal",
			state:    StateCreated,
			terminal: false,
		},
		{
			name:     "running is not terminal",
			state:    StateRunning,
			terminal: false,
		},
		{
			name:     "idle is not terminal",
			state:    StateIdle,
			terminal: false,
		},
		{
			name:     "terminated is not terminal",
			state:    StateTerminated,
			terminal: false,
		},
		{
			name:     "deleted is terminal",
			state:    StateDeleted,
			terminal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.state.IsTerminal()
			require.Equal(t, tt.terminal, result)
		})
	}
}

func TestSessionState_IsActive(t *testing.T) {
	tests := []struct {
		name   string
		state  SessionState
		active bool
	}{
		{
			name:   "created is active",
			state:  StateCreated,
			active: true,
		},
		{
			name:   "running is active",
			state:  StateRunning,
			active: true,
		},
		{
			name:   "idle is active",
			state:  StateIdle,
			active: true,
		},
		{
			name:   "terminated is not active",
			state:  StateTerminated,
			active: false,
		},
		{
			name:   "deleted is not active",
			state:  StateDeleted,
			active: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.state.IsActive()
			require.Equal(t, tt.active, result)
		})
	}
}

func TestIsValidTransition(t *testing.T) {
	tests := []struct {
		name     string
		from     SessionState
		to       SessionState
		expected bool
	}{
		// Valid transitions from Created
		{
			name:     "created to running",
			from:     StateCreated,
			to:       StateRunning,
			expected: true,
		},
		{
			name:     "created to terminated",
			from:     StateCreated,
			to:       StateTerminated,
			expected: true,
		},
		{
			name:     "created to idle is invalid",
			from:     StateCreated,
			to:       StateIdle,
			expected: false,
		},
		{
			name:     "created to deleted is invalid",
			from:     StateCreated,
			to:       StateDeleted,
			expected: false,
		},

		// Valid transitions from Running
		{
			name:     "running to idle",
			from:     StateRunning,
			to:       StateIdle,
			expected: true,
		},
		{
			name:     "running to terminated",
			from:     StateRunning,
			to:       StateTerminated,
			expected: true,
		},
		{
			name:     "running to deleted",
			from:     StateRunning,
			to:       StateDeleted,
			expected: true,
		},
		{
			name:     "running to created is invalid",
			from:     StateRunning,
			to:       StateCreated,
			expected: false,
		},

		// Valid transitions from Idle
		{
			name:     "idle to running",
			from:     StateIdle,
			to:       StateRunning,
			expected: true,
		},
		{
			name:     "idle to terminated",
			from:     StateIdle,
			to:       StateTerminated,
			expected: true,
		},
		{
			name:     "idle to deleted",
			from:     StateIdle,
			to:       StateDeleted,
			expected: true,
		},
		{
			name:     "idle to created is invalid",
			from:     StateIdle,
			to:       StateCreated,
			expected: false,
		},

		// Valid transitions from Terminated
		{
			name:     "terminated to running (resume)",
			from:     StateTerminated,
			to:       StateRunning,
			expected: true,
		},
		{
			name:     "terminated to deleted",
			from:     StateTerminated,
			to:       StateDeleted,
			expected: true,
		},
		{
			name:     "terminated to idle is invalid",
			from:     StateTerminated,
			to:       StateIdle,
			expected: false,
		},
		{
			name:     "terminated to created is invalid",
			from:     StateTerminated,
			to:       StateCreated,
			expected: false,
		},

		// No valid transitions from Deleted (terminal state)
		{
			name:     "deleted to created is invalid",
			from:     StateDeleted,
			to:       StateCreated,
			expected: false,
		},
		{
			name:     "deleted to running is invalid",
			from:     StateDeleted,
			to:       StateRunning,
			expected: false,
		},
		{
			name:     "deleted to idle is invalid",
			from:     StateDeleted,
			to:       StateIdle,
			expected: false,
		},
		{
			name:     "deleted to terminated is invalid",
			from:     StateDeleted,
			to:       StateTerminated,
			expected: false,
		},
		{
			name:     "deleted to deleted is invalid",
			from:     StateDeleted,
			to:       StateDeleted,
			expected: false,
		},

		// Self-transitions (all invalid)
		{
			name:     "created to created is invalid",
			from:     StateCreated,
			to:       StateCreated,
			expected: false,
		},
		{
			name:     "running to running is invalid",
			from:     StateRunning,
			to:       StateRunning,
			expected: false,
		},
		{
			name:     "idle to idle is invalid",
			from:     StateIdle,
			to:       StateIdle,
			expected: false,
		},
		{
			name:     "terminated to terminated is invalid",
			from:     StateTerminated,
			to:       StateTerminated,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidTransition(tt.from, tt.to)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestNewEnvelope(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		sessionID string
		seq       int64
		kind      Kind
		data      interface{}
	}{
		{
			name:      "envelope with message event",
			id:        "msg-123",
			sessionID: "session-456",
			seq:       1,
			kind:      Message,
			data: &MessageData{
				ID:      "msg-789",
				Role:    "assistant",
				Content: "Hello, world!",
			},
		},
		{
			name:      "envelope with state event",
			id:        "state-123",
			sessionID: "session-789",
			seq:       42,
			kind:      State,
			data: &StateData{
				State:   StateRunning,
				Message: "Session started",
			},
		},
		{
			name:      "envelope with error event",
			id:        "err-001",
			sessionID: "session-999",
			seq:       100,
			kind:      Error,
			data: &ErrorData{
				Code:    ErrCodeSessionNotFound,
				Message: "Session not found",
			},
		},
		{
			name:      "envelope with nil data",
			id:        "nil-data",
			sessionID: "session-nil",
			seq:       0,
			kind:      Done,
			data:      nil,
		},
		{
			name:      "envelope with zero seq",
			id:        "zero-seq",
			sessionID: "session-zero",
			seq:       0,
			kind:      Ping,
			data:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeCreate := time.Now().UnixMilli()
			envelope := NewEnvelope(tt.id, tt.sessionID, tt.seq, tt.kind, tt.data)
			afterCreate := time.Now().UnixMilli()

			// Verify Version is set correctly
			require.Equal(t, Version, envelope.Version, "Version should match package constant")

			// Verify ID is set correctly
			require.Equal(t, tt.id, envelope.ID, "ID should match input")

			// Verify SessionID is set correctly
			require.Equal(t, tt.sessionID, envelope.SessionID, "SessionID should match input")

			// Verify Seq is set correctly
			require.Equal(t, tt.seq, envelope.Seq, "Seq should match input")

			// Verify Event Type is set correctly
			require.Equal(t, tt.kind, envelope.Event.Type, "Event.Type should match input")

			// Verify Event Data is set correctly
			require.Equal(t, tt.data, envelope.Event.Data, "Event.Data should match input")

			// Verify Timestamp is set and within reasonable bounds
			require.GreaterOrEqual(t, envelope.Timestamp, beforeCreate, "Timestamp should be >= creation start time")
			require.LessOrEqual(t, envelope.Timestamp, afterCreate, "Timestamp should be <= creation end time")

			// Verify Priority is not set (zero value)
			require.Equal(t, Priority(""), envelope.Priority, "Priority should be empty/zero value")

			// Verify OwnerID is not set (zero value)
			require.Equal(t, "", envelope.OwnerID, "OwnerID should be empty")
		})
	}
}

func TestInputAckData_JSONContract(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(InputAckData{
		ClientMessageID:   "evt_client",
		ExecutionID:       "exec_1",
		Status:            ExecutionStatusDelivered,
		Duplicate:         true,
		InputMode:         InputModeInjected,
		Durability:        InputDurabilityVolatile,
		ParentExecutionID: "exec_parent",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"client_message_id":"evt_client",
		"execution_id":"exec_1",
		"status":"delivered",
		"duplicate":true,
		"input_mode":"injected",
		"durability":"volatile",
		"parent_execution_id":"exec_parent"
	}`, string(data))
}

func TestInputAckData_OldPayloadRemainsWireCompatible(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(InputAckData{
		ClientMessageID: "evt_client",
		ExecutionID:     "exec_1",
		Status:          ExecutionStatusDelivered,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"client_message_id":"evt_client",
		"execution_id":"exec_1",
		"status":"delivered"
	}`, string(data))
}

func TestIsValidTransition_InvalidStateConstant(t *testing.T) {
	// Test with an invalid/unknown state constant that's not in ValidTransitions map
	invalidState := SessionState("unknown_state")

	result := IsValidTransition(invalidState, StateRunning)
	require.False(t, result, "Transition from unknown state should return false")

	result = IsValidTransition(StateRunning, invalidState)
	require.False(t, result, "Transition to unknown state should return false")
}

func TestWorkerStdioCommandIsPassthrough(t *testing.T) {
	passthrough := []WorkerStdioCommand{
		StdioCompact, StdioClear, StdioModel,
		StdioEffort, StdioRewind, StdioCommit,
	}
	notPassthrough := []WorkerStdioCommand{
		StdioContextUsage, StdioMCPStatus,
		StdioSetModel, StdioSetPermMode,
	}
	for _, cmd := range passthrough {
		require.True(t, cmd.IsPassthrough(), string(cmd))
	}
	for _, cmd := range notPassthrough {
		require.False(t, cmd.IsPassthrough(), string(cmd))
	}
}

func TestNewEnvelope_TimestampUniqueness(t *testing.T) {
	// Create multiple envelopes and ensure timestamps are set independently
	envelope1 := NewEnvelope("id1", "session1", 1, Ping, nil)
	time.Sleep(2 * time.Millisecond) // Ensure time difference
	envelope2 := NewEnvelope("id2", "session2", 2, Pong, nil)

	// Each envelope should have a timestamp
	require.NotZero(t, envelope1.Timestamp, "First envelope should have non-zero timestamp")
	require.NotZero(t, envelope2.Timestamp, "Second envelope should have non-zero timestamp")

	// Timestamps should be different (unless created in same millisecond)
	require.GreaterOrEqual(t, envelope2.Timestamp, envelope1.Timestamp,
		"Second envelope timestamp should be >= first envelope timestamp")
}

func TestClone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		envelope *Envelope
	}{
		{
			name: "normal envelope",
			envelope: &Envelope{
				ID:        "test-id",
				Version:   Version,
				Seq:       42,
				Timestamp: 1234567890,
				SessionID: "session-123",
				Priority:  PriorityData,
				OwnerID:   "user-123",
				Event: Event{
					Type: Message,
					Data: &MessageData{
						ID:      "msg-456",
						Role:    "assistant",
						Content: "Hello, world!",
					},
				},
			},
		},
		{
			name: "envelope with nested data",
			envelope: &Envelope{
				ID:        "state-id",
				Version:   Version,
				Seq:       99,
				Timestamp: 9876543210,
				SessionID: "session-456",
				Priority:  PriorityControl,
				OwnerID:   "user-456",
				Event: Event{
					Type: State,
					Data: &StateData{
						State:   StateRunning,
						Message: "Session started successfully",
					},
				},
			},
		},
		{
			name: "envelope with nil data",
			envelope: &Envelope{
				ID:        "nil-data-id",
				Version:   Version,
				Seq:       0,
				Timestamp: 1111111111,
				SessionID: "session-nil",
				Priority:  "",
				OwnerID:   "",
				Event: Event{
					Type: Ping,
					Data: nil,
				},
			},
		},
		{
			name: "envelope with map data",
			envelope: &Envelope{
				ID:        "map-data-id",
				Version:   Version,
				Seq:       77,
				Timestamp: 2222222222,
				SessionID: "session-map",
				Priority:  PriorityData,
				OwnerID:   "user-map",
				Event: Event{
					Type: ContextUsage,
					Data: map[string]any{
						"totalTokens": float64(1000),
						"maxTokens":   float64(2000),
						"model":       "test-model",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clone := Clone(tt.envelope)

			require.Equal(t, tt.envelope.ID, clone.ID)
			require.Equal(t, tt.envelope.Version, clone.Version)
			require.Equal(t, tt.envelope.Seq, clone.Seq)
			require.Equal(t, tt.envelope.Timestamp, clone.Timestamp)
			require.Equal(t, tt.envelope.SessionID, clone.SessionID)
			require.Equal(t, tt.envelope.Priority, clone.Priority)
			require.Equal(t, tt.envelope.Event.Type, clone.Event.Type)

			// For complex data types, compare the actual values
			// JSON marshal/unmarshal may produce different field ordering
			if tt.envelope.Event.Data == nil {
				require.Nil(t, clone.Event.Data)
			} else {
				// Use JSON equality for complex types
				originalJSON, err := json.Marshal(tt.envelope.Event.Data)
				require.NoError(t, err)
				cloneJSON, err := json.Marshal(clone.Event.Data)
				require.NoError(t, err)

				var originalUnmarshaled, cloneUnmarshaled interface{}
				err = json.Unmarshal(originalJSON, &originalUnmarshaled)
				require.NoError(t, err)
				err = json.Unmarshal(cloneJSON, &cloneUnmarshaled)
				require.NoError(t, err)

				require.Equal(t, originalUnmarshaled, cloneUnmarshaled)
			}

			originalID := tt.envelope.ID
			tt.envelope.ID = "modified-id"
			require.NotEqual(t, tt.envelope.ID, clone.ID)
			require.Equal(t, originalID, clone.ID)

			tt.envelope.ID = originalID
		})
	}
}

func TestClone_EmptyEnvelope(t *testing.T) {
	t.Parallel()

	// Test with nil envelope (edge case)
	result := Clone(nil)
	require.NotNil(t, result)
	require.Equal(t, &Envelope{}, result)

	// Test with zero-value envelope
	emptyEnv := &Envelope{}
	clone := Clone(emptyEnv)
	require.NotNil(t, clone)
	require.Equal(t, emptyEnv, clone)
	require.NotSame(t, emptyEnv, clone) // Should be different pointer
}

func TestQuestion_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		jsonStr  string
		expected bool
	}{
		{
			name:     "unmarshal standard multi_select key",
			jsonStr:  `{"question": "test?", "header": "test", "multi_select": true}`,
			expected: true,
		},
		{
			name:     "unmarshal standard multi_select false",
			jsonStr:  `{"question": "test?", "header": "test", "multi_select": false}`,
			expected: false,
		},
		{
			name:     "unmarshal alias is_multi_select key",
			jsonStr:  `{"question": "test?", "header": "test", "is_multi_select": true}`,
			expected: true,
		},
		{
			name:     "unmarshal alias multiple key",
			jsonStr:  `{"question": "test?", "header": "test", "multiple": true}`,
			expected: true,
		},
		{
			name:     "unmarshal default false when none provided",
			jsonStr:  `{"question": "test?", "header": "test"}`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var q Question
			err := json.Unmarshal([]byte(tt.jsonStr), &q)
			require.NoError(t, err)
			require.Equal(t, tt.expected, q.MultiSelect)
		})
	}
}
