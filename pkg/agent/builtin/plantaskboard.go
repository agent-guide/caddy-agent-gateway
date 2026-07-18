package builtin

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"sync"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/plantask"
)

// planTaskBaseDir is the virtual directory the plantask middleware works in.
// It only namespaces paths inside one board; there is no filesystem behind it.
const planTaskBaseDir = "tasks"

// The board is model-controlled in-memory state, so both object count and
// bytes are bounded. The total cap is deliberately much smaller than
// maxPlanTaskFiles*maxPlanTaskFileBytes: many individually valid files must
// not combine into an unbounded session allocation.
const (
	maxPlanTaskFiles      = 256
	maxPlanTaskFileBytes  = 256 << 10
	maxPlanTaskBoardBytes = 1 << 20
)

// planTaskBoard is one session's in-memory task storage: virtual path →
// content. It lives on the session object, so it is evicted with the session
// and shares its restart-loss semantics.
type planTaskBoard struct {
	mu         sync.Mutex
	files      map[string]string
	totalBytes int
}

func newPlanTaskBoard() *planTaskBoard {
	return &planTaskBoard{files: map[string]string{}}
}

type planTaskBoardCtxKey struct{}

// withTaskBoard binds the turn's session task board into the context the ADK
// run (and therefore every plantask tool execution) receives.
func withTaskBoard(ctx context.Context, b *planTaskBoard) context.Context {
	return context.WithValue(ctx, planTaskBoardCtxKey{}, b)
}

func taskBoardFromContext(ctx context.Context) (*planTaskBoard, bool) {
	b, ok := ctx.Value(planTaskBoardCtxKey{}).(*planTaskBoard)
	return b, ok
}

// planTaskBackend implements plantask.Backend against the session board
// carried by the turn context. The middleware instance is cached per agent
// materialization, so the backend must be stateless: per-session state comes
// exclusively from the context.
type planTaskBackend struct{}

func (planTaskBackend) board(ctx context.Context) (*planTaskBoard, error) {
	b, ok := taskBoardFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("plantask board is not bound to this turn context")
	}
	return b, nil
}

func (be planTaskBackend) LsInfo(ctx context.Context, req *plantask.LsInfoRequest) ([]plantask.FileInfo, error) {
	b, err := be.board(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []plantask.FileInfo
	for p, content := range b.files {
		if path.Dir(p) != req.Path {
			continue
		}
		out = append(out, plantask.FileInfo{Path: p, Size: int64(len(content))})
	}
	// Map iteration order is random; keep listings deterministic.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (be planTaskBackend) Read(ctx context.Context, req *plantask.ReadRequest) (*filesystem.FileContent, error) {
	b, err := be.board(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	content, ok := b.files[req.FilePath]
	if !ok {
		return nil, fmt.Errorf("plantask file %q: %w", req.FilePath, os.ErrNotExist)
	}
	return &filesystem.FileContent{Content: content}, nil
}

func (be planTaskBackend) Write(ctx context.Context, req *plantask.WriteRequest) error {
	b, err := be.board(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	contentBytes := len(req.Content)
	if contentBytes > maxPlanTaskFileBytes {
		return fmt.Errorf("plantask file %q is too large (%d bytes; maximum %d)", req.FilePath, contentBytes, maxPlanTaskFileBytes)
	}
	if _, exists := b.files[req.FilePath]; !exists && len(b.files) >= maxPlanTaskFiles {
		return fmt.Errorf("plantask board is full (%d files)", maxPlanTaskFiles)
	}
	oldBytes := len(b.files[req.FilePath])
	nextTotal := b.totalBytes - oldBytes + contentBytes
	if nextTotal > maxPlanTaskBoardBytes {
		return fmt.Errorf("plantask board is full (%d bytes; maximum %d)", nextTotal, maxPlanTaskBoardBytes)
	}
	b.files[req.FilePath] = req.Content
	b.totalBytes = nextTotal
	return nil
}

func (be planTaskBackend) Delete(ctx context.Context, req *plantask.DeleteRequest) error {
	b, err := be.board(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	content, ok := b.files[req.FilePath]
	if !ok {
		return fmt.Errorf("plantask file %q: %w", req.FilePath, os.ErrNotExist)
	}
	delete(b.files, req.FilePath)
	b.totalBytes -= len(content)
	return nil
}
