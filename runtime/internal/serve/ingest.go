package serve

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// handleRunIngest accepts a JSONL stream of trace events from an existing run
// and writes them to a new run directory.
func (s *Server) handleRunIngest(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/x-ndjson") && !strings.Contains(ct, "application/jsonl") {
		http.Error(w, "Content-Type must be application/x-ndjson or application/jsonl", http.StatusBadRequest)
		return
	}

	// Generate a new run ID for this ingested run
	runID := uuid.New().String()

	// Create run directory
	runDir := filepath.Join(s.getRunsDir(), runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create run directory: %v", err), http.StatusInternalServerError)
		return
	}

	// Create evidence subdirectory
	evidenceDir := filepath.Join(runDir, "evidence")
	if err := os.MkdirAll(evidenceDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create evidence directory: %v", err), http.StatusInternalServerError)
		return
	}

	// Open trace file for writing
	tracePath := filepath.Join(runDir, "trace.jsonl")
	traceFile, err := os.Create(tracePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create trace file: %v", err), http.StatusInternalServerError)
		return
	}
	defer traceFile.Close()

	scanner := bufio.NewScanner(r.Body)
	var eventsReceived int64
	var firstEvent bool = true

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse event to validate it's valid JSON
		var event tracepkg.TraceEvent
		if err := json.Unmarshal(line, &event); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON at line %d: %v", eventsReceived+1, err), http.StatusBadRequest)
			// Clean up partially written run
			_ = os.RemoveAll(runDir)
			return
		}

		// First event must be run/started
		if firstEvent {
			if event.Kind != tracepkg.EventKindRunStarted {
				http.Error(w, "first event must be run/started", http.StatusBadRequest)
				_ = os.RemoveAll(runDir)
				return
			}
			firstEvent = false
		}

		// Write event to trace file
		if _, err := traceFile.Write(line); err != nil {
			http.Error(w, fmt.Sprintf("failed to write event: %v", err), http.StatusInternalServerError)
			_ = os.RemoveAll(runDir)
			return
		}
		if _, err := traceFile.WriteString("\n"); err != nil {
			http.Error(w, fmt.Sprintf("failed to write newline: %v", err), http.StatusInternalServerError)
			_ = os.RemoveAll(runDir)
			return
		}

		eventsReceived++
	}

	if err := scanner.Err(); err != nil {
		http.Error(w, fmt.Sprintf("error reading request body: %v", err), http.StatusBadRequest)
		_ = os.RemoveAll(runDir)
		return
	}

	if eventsReceived == 0 {
		http.Error(w, "no events in stream", http.StatusBadRequest)
		_ = os.RemoveAll(runDir)
		return
	}

	// Sync trace file to disk
	if err := traceFile.Sync(); err != nil {
		http.Error(w, fmt.Sprintf("failed to sync trace file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"run_id":          runID,
		"events_received": eventsReceived,
	})
}

// handleRunAttachment handles POST /api/yawr/v1/runs/{run-id}/attachments/{sha256}
// Accepts any content type and saves it to the run's evidence directory.
func (s *Server) handleRunAttachment(w http.ResponseWriter, r *http.Request) {
	// Extract run-id and sha256 from path
	// Path format: /api/yawr/v1/runs/{run-id}/attachments/{sha256}
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/yawr/v1/runs/"), "/")
	if len(pathParts) != 3 || pathParts[1] != "attachments" {
		http.Error(w, "invalid path format", http.StatusBadRequest)
		return
	}

	runID := pathParts[0]
	expectedSHA := pathParts[2]

	// Validate run exists
	runDir := filepath.Join(s.getRunsDir(), runID)
	if _, err := os.Stat(runDir); os.IsNotExist(err) {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	// Create evidence directory if it doesn't exist
	evidenceDir := filepath.Join(runDir, "evidence")
	if err := os.MkdirAll(evidenceDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create evidence directory: %v", err), http.StatusInternalServerError)
		return
	}

	// Read body and compute SHA-256
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
		return
	}

	hash := sha256.Sum256(body)
	actualSHA := hex.EncodeToString(hash[:])

	// Validate SHA-256 matches
	if actualSHA != expectedSHA {
		http.Error(w, fmt.Sprintf("SHA-256 mismatch: expected %s, got %s", expectedSHA, actualSHA), http.StatusBadRequest)
		return
	}

	// Write file to evidence directory
	attachmentPath := filepath.Join(evidenceDir, expectedSHA)
	if err := os.WriteFile(attachmentPath, body, 0644); err != nil {
		http.Error(w, fmt.Sprintf("failed to write attachment: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("OK"))
}

// getRunsDir returns the base directory for run storage.
// This should use the same directory as the engine's RunStore.
func (s *Server) getRunsDir() string {
	// Default to .runbook/runs if store doesn't expose the path
	// TODO: This should be extracted from the engine config's Store
	return ".runbook/runs"
}
