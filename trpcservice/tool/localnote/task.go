package localnote

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

// TaskRequirements recognizes an explicit business command, never heuristics
// over ordinary conversation. Confirmation is still mandatory before writing.
func TaskRequirements(text string) ([]taskflow.Requirement, error) {
	text = strings.TrimSpace(text)
	if text != "/note" && !strings.HasPrefix(text, "/note ") && !strings.HasPrefix(text, "/note\n") {
		return nil, nil
	}
	args := []byte(strings.TrimSpace(strings.TrimPrefix(text, "/note")))
	if len(args) == 0 || len(args) > maximumArguments {
		return nil, runtime.ErrInvalidEnvelope
	}
	canonical, digest, err := governance.CanonicalArguments(args)
	if err != nil {
		return nil, runtime.ErrInvalidEnvelope
	}
	var in input
	d := json.NewDecoder(bytes.NewReader(canonical))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Content) == "" || len(strings.TrimSpace(in.Title)) > maximumTitle || len(strings.TrimSpace(in.Content)) > maximumContent || strings.ContainsRune(in.Title, 0) || strings.ContainsRune(in.Content, 0) {
		return nil, runtime.ErrInvalidEnvelope
	}
	return []taskflow.Requirement{{ID: "create_requested_note", Description: "Create exactly the note described by the original /note JSON after user confirmation, read back the persisted record, and return its real note_id. Preserve the original JSON string values when calling the tool.", ToolID: ID, ToolVersion: Version, ArgsDigest: digest, EvidencePath: []string{"note_id"}}}, nil
}
