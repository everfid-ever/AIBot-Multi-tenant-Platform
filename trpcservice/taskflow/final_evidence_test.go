package taskflow

import (
	"errors"
	"testing"
)

func TestBusinessReferencesNeedExactASCIIIdentifierAndScalarReceipt(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   bool
	}{{"created note_abc", true}, {"已创建note_abc。", true}, {"`note_abc`", true}, {"created note_abc_extra", false}, {"othernote_abc", false}} {
		if got := containsBusinessReference(tc.answer, "note_abc"); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	checks := []*ResultCheck{{Path: []string{"id"}, NonEmpty: true}}
	for _, b := range []string{`{"id":"note_abc"}`, `{"id":9007199254740993}`} {
		if err := VerifyFinalEvidenceReceipt(checks, []byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []string{`{}`, `{"id":true}`, `{"id":{}}`, `{"id":""}`} {
		if err := VerifyFinalEvidenceReceipt(checks, []byte(b)); !errors.Is(err, ErrEvidenceRejected) {
			t.Fatal(b, err)
		}
	}
}
