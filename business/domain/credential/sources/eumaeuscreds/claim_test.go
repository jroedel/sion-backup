package eumaeuscreds_test

import (
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/credential/sources/eumaeuscreds"
)

// TestARefusedCodeSaysWhatToDo is the one sentence about enrolment codes that
// somebody is guaranteed to read. It used to say only that the code was not
// valid, which leaves a person holding a dead code with nothing to try.
func TestARefusedCodeSaysWhatToDo(t *testing.T) {
	msg := eumaeuscreds.ErrCodeUnknown.Error()

	for _, want := range []string{"expired", "fifteen minutes", "once", "another"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %s", want, msg)
		}
	}
}
