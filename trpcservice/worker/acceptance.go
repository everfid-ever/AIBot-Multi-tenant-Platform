package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// TaskRequirementResolver derives reviewed business acceptance conditions from
// the pinned execution configuration/input. It runs only before first planning;
// takeover reuses the frozen contract, even when configuration has changed.
type TaskRequirementResolver func(context.Context, runtime.ExecutionEnvelope, model.Message) ([]taskflow.Requirement, error)

func (w RunnerExecutor) bindTaskAcceptance(ctx context.Context, task *taskflow.Session, e runtime.ExecutionEnvelope, message model.Message, policy map[string]any) error {
	p, err := task.Store.Get(ctx, task.Scope.Key)
	if err != nil {
		return err
	}
	if p.Acceptance != nil {
		if p.Acceptance.InputDigest != p.InputDigest {
			return runtime.ErrVersionMismatch
		}
		return p.Acceptance.Validate()
	}
	// Retain the interpretation of candidates and plans created before this feature.
	if p.Phase != "open" || p.Attempts != 0 || len(p.Steps) != 0 {
		return nil
	}
	objective := message.Content
	if strings.TrimSpace(objective) == "" {
		objective = "Respond to the original persisted multimodal user input."
	}
	// Full input remains authoritative in payload storage, bound by InputDigest.
	if len(objective) > 60<<10 {
		end := 60 << 10
		for !utf8.ValidString(objective[:end]) {
			end--
		}
		objective = objective[:end] + "\n[Objective excerpt; full original input is retained under input_digest.]"
	}
	a := taskflow.Acceptance{Version: 1, InputDigest: p.InputDigest, Objective: objective}
	if configured, ok := policy["task_requirements"]; ok {
		b, marshalErr := json.Marshal(configured)
		if marshalErr != nil || len(b) > 64<<10 || string(b) == "null" {
			return runtime.ErrInvalidEnvelope
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err = d.Decode(&a.Requirements); err != nil {
			return runtime.ErrInvalidEnvelope
		}
	}
	if w.TaskRequirements != nil {
		extra, resolveErr := w.TaskRequirements(ctx, e, message)
		if resolveErr != nil {
			return resolveErr
		}
		a.Requirements = append(a.Requirements, extra...)
	}
	_, err = task.BindAcceptance(ctx, a)
	return err
}
