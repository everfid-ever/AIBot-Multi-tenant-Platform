package session

import (
	"context"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// TerminalHistory is coordination metadata, never a second conversation store.
type TerminalHistory struct {
	RequestID, CommitID, ResultRef string
	InputSeq, Fence                uint64
	Outcome                        runtime.Outcome
	CommittedAt                    time.Time
}

const MaxHistoryTurns = 100

type TerminalHistoryReader interface {
	// ListTerminalHistory returns the newest bounded window strictly BEFORE
	// beforeInputSeq, in ascending input order, scoped to one session.
	ListTerminalHistory(context.Context, SessionKey, uint64, int) ([]TerminalHistory, error)
}

// CommittedHistoryTurn is a verified read projection of existing input/result
// payloads and terminal commits. It is not written back to SDK event storage.
type CommittedHistoryTurn struct {
	TerminalHistory
	Input  *model.Message
	Answer model.Message
}

func (t *BufferedTurn) ConfigureCommittedHistory(turns []CommittedHistoryTurn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.committedHistory = make([]CommittedHistoryTurn, len(turns))
	for i, turn := range turns {
		copy := turn
		copy.Answer = cloneHistoryMessage(turn.Answer)
		if turn.Input != nil {
			input := cloneHistoryMessage(*turn.Input)
			copy.Input = &input
		}
		t.committedHistory[i] = copy
	}
	t.authoritativeHistory = true
}

func cloneHistoryMessage(m model.Message) model.Message {
	return (&model.Response{Choices: []model.Choice{{Message: m}}}).Clone().Choices[0].Message
}
