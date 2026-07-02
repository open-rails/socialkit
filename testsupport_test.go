package socialkit

import (
	"context"
	"fmt"
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
