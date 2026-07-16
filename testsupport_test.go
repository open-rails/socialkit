package socialkit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testSchema is the host schema the kit's tables land in during tests.
const testSchema = "hostapp"

// newTestRuntime spins up a throwaway Postgres, builds a Runtime against the
// given (usually fake) ports, and returns it plus the pool. Shared by every
// module's tests. Skips if Docker is unavailable.
func newTestRuntime(t *testing.T, opts Options) (*Runtime, *pgxpool.Pool) {
	t.Helper()
	pool := newTestPool(t)
	opts.Pool = pool
	opts.Schema = testSchema
	if opts.Identity == nil {
		opts.Identity = &fakeIdentity{}
	}
	if opts.Authz == nil {
		opts.Authz = allowAll{}
	}
	if opts.Entities == nil {
		opts.Entities = &fakeResolver{}
	}
	rt, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New runtime: %v", err)
	}
	return rt, pool
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("hostdb"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("cannot start postgres testcontainer (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	dsn := fmt.Sprintf("postgresql://super:super@%s:%s/hostdb?sslmode=disable", host, port.Port())
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --- fake ports ---

// fakeIdentity returns a fixed actor set per-request via WithActor on context.
type fakeIdentity struct{}

type actorCtxKey struct{}

func withActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

func (*fakeIdentity) Actor(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(Actor)
	return a, ok
}

// allowAll authorizes every perm; denyAll denies; errAuthz fails (fail-closed).
type allowAll struct{}

func (allowAll) Can(context.Context, Actor, string) (bool, error) { return true, nil }

type denyAll struct{}

func (denyAll) Can(context.Context, Actor, string) (bool, error) { return false, nil }

// fakeResolver answers from an in-memory map keyed by "type:id"; missing => not
// found. Visible/Accessible default true unless overridden.
type fakeResolver struct {
	mu      sync.Mutex
	entries map[string]EntityRef
}

func (f *fakeResolver) set(entityType, id string, visible, accessible bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]EntityRef{}
	}
	f.entries[entityType+":"+id] = EntityRef{Type: entityType, ID: id, Visible: visible, Accessible: accessible}
}

func (f *fakeResolver) Resolve(_ context.Context, entityType, id string, _ Actor) (EntityRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref, ok := f.entries[entityType+":"+id]
	if !ok {
		return EntityRef{}, ErrNotFound
	}
	return ref, nil
}

// fakeMedia is an in-memory MediaStore capturing uploads for assertions.
type fakeMedia struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func (f *fakeMedia) Put(_ context.Context, key string, data []byte, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = data
	return "https://cdn.test/" + key, nil
}

// DeleteByURL mirrors s3Store's optional mediaURLDeleter: URLs outside the
// fake origin are ignored; matching keys are removed from the store.
func (f *fakeMedia) DeleteByURL(_ context.Context, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := strings.CutPrefix(url, "https://cdn.test/")
	if !ok {
		return nil
	}
	delete(f.puts, key)
	return nil
}

func (f *fakeMedia) stored(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.puts[key]
	return d, ok
}

func (f *fakeMedia) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// recordingRecorder captures emitted signals for assertions.
type recordingRecorder struct {
	mu        sync.Mutex
	reactions []ReactionSignal
	posts     []PostSignal
}

type committedStateRecorder struct {
	mu      sync.Mutex
	pool    *pgxpool.Pool
	signals []ReactionSignal
	errs    []error
}

func (r *committedStateRecorder) Reaction(ctx context.Context, signal ReactionSignal) {
	var err error
	switch signal.Kind {
	case "favorite", "unfavorite":
		var exists bool
		err = r.pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM hostapp.social_favorites
			WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3
		)`, signal.ActorID, signal.EntityType, signal.EntityID).Scan(&exists)
		wantExists := signal.Kind == "favorite"
		if err == nil && exists != wantExists {
			err = fmt.Errorf("favorite row existence = %t, want %t", exists, wantExists)
		}
	default:
		var value int16
		err = r.pool.QueryRow(ctx, `SELECT value FROM hostapp.social_reactions
			WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3`,
			signal.ActorID, signal.EntityType, signal.EntityID).Scan(&value)
		if err == nil {
			expected := int16(0)
			switch signal.Kind {
			case "like":
				expected = 1
			case "dislike":
				expected = -1
			}
			if value != expected {
				err = fmt.Errorf("reaction value = %d, want %d", value, expected)
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, signal)
	if err != nil {
		r.errs = append(r.errs, err)
	}
}

func (*committedStateRecorder) Post(context.Context, PostSignal) {}

func (r *committedStateRecorder) assertVisible(t *testing.T, wantSignals int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.signals) != wantSignals {
		t.Fatalf("recorder signals = %d, want %d", len(r.signals), wantSignals)
	}
	if len(r.errs) != 0 {
		t.Fatalf("recorder could not observe committed state: %v", r.errs)
	}
}

func (r *recordingRecorder) Reaction(_ context.Context, s ReactionSignal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = append(r.reactions, s)
}

func (r *recordingRecorder) Post(_ context.Context, s PostSignal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, s)
}

func (r *recordingRecorder) reactionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reactions)
}

func (r *recordingRecorder) reactionSignals() []ReactionSignal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReactionSignal(nil), r.reactions...)
}

func (r *recordingRecorder) resetReactions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = nil
}

// reactErr adapts react's (ref, error) return for error-only test assertions.
func reactErr(_ EntityRef, err error) error { return err }
