//go:build dev

package guest

import "testing"

// harvest answers with the agent's skill files; its limit is the dev build's own.
func TestHarvestMayAnswerWithFiles(t *testing.T) {
	if got := answerLimit("harvest", nil); got != MaxHarvestAnswer {
		t.Fatalf("harvest limit %d", got)
	}
	if got := answerLimit("update", []string{"read"}); got != MaxFilesAnswer {
		t.Fatalf("update read limit %d", got)
	}
}
