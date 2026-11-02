package taskflow

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

// ParseRequirements is shared by publication and execution. Policy is trusted
// versioned application configuration, never model-supplied checkpoint data.
func ParseRequirements(policy map[string]any) ([]Requirement, error) {
	v, ok := policy["task_requirements"]
	if !ok {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > 64<<10 || string(b) == "null" {
		return nil, runtime.ErrInvalidEnvelope
	}
	var requirements []Requirement
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&requirements); err != nil {
		return nil, runtime.ErrInvalidEnvelope
	}
	a := Acceptance{Version: 1, InputDigest: strings.Repeat("0", 64), Objective: "configuration validation", Requirements: requirements}
	if err = a.Validate(); err != nil {
		return nil, err
	}
	return requirements, nil
}
