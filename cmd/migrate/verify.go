package migrate

import (
	"context"
	"database/sql"
	_ "embed"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/zitadel/logging"

	"github.com/zitadel/zitadel/internal/database"
)

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "counts if source and dest have the same amount of entries",
		Run: func(cmd *cobra.Command, args []string) {
			config := mustNewMigrationConfig(viper.GetViper())
			verifyMigration(cmd.Context(), config)
		},
	}
}

var schemas = []string{
	"adminapi",
	"auth",
	"eventstore",
	"projections",
	"system",
}

func verifyMigration(ctx context.Context, config *Migration) {
	sourceClient, err := database.Connect(config.Source, false, false)
	logging.OnError(err).Fatal("unable to connect to source database")
	defer sourceClient.Close()

	destClient, err := database.Connect(config.Destination, false, true)
	logging.OnError(err).Fatal("unable to connect to destination database")
	defer destClient.Close()

	for _, schema := range schemas {
		for _, table := range append(getTables(ctx, destClient, schema), getViews(ctx, destClient, schema)...) {
			sourceCount := countEntries(ctx, sourceClient, table)
			destCount := countEntries(ctx, destClient, table)

			entry := logging.WithFields("table", table, "dest", destCount, "source", sourceCount)
			if sourceCount == destCount {
				entry.Debug("equal count")
				continue
			}
			entry.WithField("diff", destCount-sourceCount).Error("unequal count")
		}
	}
}

func getTables(ctx context.Context, dest *database.DB, schema string) (tables []string) {
	err := dest.Query(
		func(r *sql.Rows) error {
			for r.Next() {
				var table string
				if err := r.Scan(&table); err != nil {
					return err
				}
				tables = append(tables, table)
			}
			return r.Err()
		},
		"SELECT CONCAT(schemaname, '.', tablename) FROM pg_tables WHERE schemaname = $1",
		schema,
	)
	logging.WithFields("schema", schema).OnError(err).Fatal("unable to query tables")
	return tables
}

func getViews(ctx context.Context, dest *database.DB, schema string) (tables []string) {
	err := dest.Query(
		func(r *sql.Rows) error {
			for r.Next() {
				var table string
				if err := r.Scan(&table); err != nil {
					return err
				}
				tables = append(tables, table)
			}
			return r.Err()
		},
		"SELECT CONCAT(schemaname, '.', viewname) FROM pg_views WHERE schemaname = $1",
		schema,
	)
	logging.WithFields("schema", schema).OnError(err).Fatal("unable to query views")
	return tables
}

func countEntries(ctx context.Context, client *database.DB, table string) (count int) {
	err := client.QueryRow(
		func(r *sql.Row) error {
			return r.Scan(&count)
		},
		"SELECT COUNT(*) FROM "+table,
	)
	logging.WithFields("table", table, "db", client.DatabaseName()).OnError(err).Error("unable to count")

	return count
}
