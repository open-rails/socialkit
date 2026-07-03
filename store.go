package socialkit

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// querier is the subset of pgx shared by *pgxpool.Pool and pgx.Tx, so store
// helpers run either standalone or inside a caller's transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// store holds the shared pool and the pre-qualified, schema-scoped table names.
// Every module references tables via s.t.* so all queries land in the host
// schema given at construction (no reliance on the pool's search_path).
type store struct {
	pool   *pgxpool.Pool
	schema string
	t      tables
}

// tables are the fully-qualified ("schema"."name") identifiers, sanitized once.
type tables struct {
	reactions     string
	comments      string
	pollQuestions string
	pollOptions   string
	pollVotes     string
	posts         string
	favorites     string
	entityCounts  string
}

func newStore(pool *pgxpool.Pool, schema string) *store {
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	return &store{
		pool:   pool,
		schema: schema,
		t: tables{
			reactions:     q("social_reactions"),
			comments:      q("social_comments"),
			pollQuestions: q("social_poll_questions"),
			pollOptions:   q("social_poll_options"),
			pollVotes:     q("social_poll_votes"),
			posts:         q("social_posts"),
			favorites:     q("social_favorites"),
			entityCounts:  q("social_entity_counts"),
		},
	}
}
