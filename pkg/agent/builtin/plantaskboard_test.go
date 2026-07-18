package builtin

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk/middlewares/plantask"
)

func TestPlanTaskBoardEnforcesFileAndTotalByteLimits(t *testing.T) {
	board := newPlanTaskBoard()
	backend := planTaskBackend{}
	ctx := withTaskBoard(t.Context(), board)

	write := func(name, content string) error {
		return backend.Write(ctx, &plantask.WriteRequest{FilePath: name, Content: content})
	}
	chunk := strings.Repeat("x", maxPlanTaskFileBytes)
	for i, name := range []string{"a", "b", "c", "d"} {
		if err := write(name, chunk); err != nil {
			t.Fatalf("write chunk %d error = %v", i, err)
		}
	}
	if board.totalBytes != maxPlanTaskBoardBytes {
		t.Fatalf("totalBytes = %d, want %d", board.totalBytes, maxPlanTaskBoardBytes)
	}

	if err := write("too-large", strings.Repeat("x", maxPlanTaskFileBytes+1)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized write error = %v, want per-file limit rejection", err)
	}
	if err := write("over-total", "x"); err == nil || !strings.Contains(err.Error(), "board is full") {
		t.Fatalf("over-total write error = %v, want total-byte limit rejection", err)
	}
	if _, exists := board.files["over-total"]; exists {
		t.Fatal("failed over-total write modified the board")
	}
}

func TestPlanTaskBoardOverwriteAndDeleteReleaseBytes(t *testing.T) {
	board := newPlanTaskBoard()
	backend := planTaskBackend{}
	ctx := withTaskBoard(t.Context(), board)

	write := func(name, content string) error {
		return backend.Write(ctx, &plantask.WriteRequest{FilePath: name, Content: content})
	}
	if err := write("task", strings.Repeat("x", maxPlanTaskFileBytes)); err != nil {
		t.Fatalf("initial write error = %v", err)
	}
	if err := write("task", "small"); err != nil {
		t.Fatalf("smaller overwrite error = %v", err)
	}
	if board.totalBytes != len("small") {
		t.Fatalf("totalBytes after overwrite = %d, want %d", board.totalBytes, len("small"))
	}
	if err := backend.Delete(ctx, &plantask.DeleteRequest{FilePath: "task"}); err != nil {
		t.Fatalf("delete error = %v", err)
	}
	if board.totalBytes != 0 {
		t.Fatalf("totalBytes after delete = %d, want 0", board.totalBytes)
	}
}
