package migrate_test

// A Postgres testcontainer per test: these tests migrate to an intermediate
// version and plant rows that later migrations refuse, so they cannot share a
// database with each other or with anything else.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func freshDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:17",
		postgres.WithDatabase("typemore"),
		postgres.WithUsername("typemore"),
		postgres.WithPassword("typemore"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err, "start postgres testcontainer")
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return dsn
}
