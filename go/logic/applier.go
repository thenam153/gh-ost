/*
   Copyright 2025 GitHub Inc.
	 See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package logic

import (
	gosql "database/sql"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/github/gh-ost/go/base"
	"github.com/github/gh-ost/go/binlog"
	"github.com/github/gh-ost/go/sql"

	"context"
	"database/sql/driver"

	"errors"
	"sync"

	"github.com/github/gh-ost/go/mysql"
	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/openark/golib/sqlutils"
)

const (
	GhostChangelogTableComment = "gh-ost changelog"
	atomicCutOverMagicHint     = "ghost-cut-over-sentry"
)

// ErrNoCheckpointFound is returned when an empty checkpoint table is queried.
var ErrNoCheckpointFound = errors.New("no checkpoint found in _ghk table")

// MySQL error codes for foreign key constraints
const (
	MySQLErrFKConstraintFails   = 1452 // Cannot add or update a child row: a foreign key constraint fails
	MySQLErrFKConstrParentFails = 1451 // Cannot delete or update a parent row: a foreign key constraint fails
)

// isForeignKeyError checks if the error is a MySQL foreign key constraint error
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *drivermysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == MySQLErrFKConstraintFails || mysqlErr.Number == MySQLErrFKConstrParentFails
	}
	return false
}

type dmlBuildResult struct {
	query     string
	args      []interface{}
	rowsDelta int64
	err       error
}

func newDmlBuildResult(query string, args []interface{}, rowsDelta int64, err error) *dmlBuildResult {
	return &dmlBuildResult{
		query:     query,
		args:      args,
		rowsDelta: rowsDelta,
		err:       err,
	}
}

func newDmlBuildResultError(err error) *dmlBuildResult {
	return &dmlBuildResult{
		err: err,
	}
}

// Applier connects and writes the applier-server, which is the server where migration
// happens. This is typically the master, but could be a replica when `--test-on-replica` or
// `--execute-on-replica` are given.
// Applier is the one to actually write row data and apply binlog events onto the ghost table.
// It is where the ghost & changelog tables get created. It is where the cut-over phase happens.
type Applier struct {
	connectionConfig  *mysql.ConnectionConfig
	db                *gosql.DB
	singletonDB       *gosql.DB
	migrationContext  *base.MigrationContext
	finishedMigrating int64
	name              string

	CurrentCoordinatesMutex sync.Mutex
	CurrentCoordinates      mysql.BinlogCoordinates

	LastIterationRangeMutex     sync.Mutex
	LastIterationRangeMinValues *sql.ColumnValues
	LastIterationRangeMaxValues *sql.ColumnValues

	dmlDeleteQueryBuilder        *sql.DMLDeleteQueryBuilder
	dmlInsertQueryBuilder        *sql.DMLInsertQueryBuilder
	dmlUpdateQueryBuilder        *sql.DMLUpdateQueryBuilder
	checkpointInsertQueryBuilder *sql.CheckpointInsertQueryBuilder

	// FK fallback: track PKs that had FK errors and need special handling
	fkTrackedPKs    map[string]bool // Key: serialized PK values
	fkTrackedPKsMux sync.RWMutex    // Mutex for concurrent access
}

func NewApplier(migrationContext *base.MigrationContext) *Applier {
	return &Applier{
		connectionConfig:  migrationContext.ApplierConnectionConfig,
		migrationContext:  migrationContext,
		finishedMigrating: 0,
		name:              "applier",
		fkTrackedPKs:      make(map[string]bool),
	}
}

func (this *Applier) InitDBConnections() (err error) {
	applierUri := this.connectionConfig.GetDBUri(this.migrationContext.DatabaseName)
	uriWithMulti := fmt.Sprintf("%s&multiStatements=true", applierUri)
	if this.db, _, err = mysql.GetDB(this.migrationContext.Uuid, uriWithMulti); err != nil {
		return err
	}
	singletonApplierUri := fmt.Sprintf("%s&timeout=0", applierUri)
	if this.singletonDB, _, err = mysql.GetDB(this.migrationContext.Uuid, singletonApplierUri); err != nil {
		return err
	}
	this.singletonDB.SetMaxOpenConns(1)
	version, err := base.ValidateConnection(this.db, this.connectionConfig, this.migrationContext, this.name)
	if err != nil {
		return err
	}
	if _, err := base.ValidateConnection(this.singletonDB, this.connectionConfig, this.migrationContext, this.name); err != nil {
		return err
	}
	this.migrationContext.ApplierMySQLVersion = version
	if err := this.validateAndReadGlobalVariables(); err != nil {
		return err
	}
	if !this.migrationContext.AliyunRDS && !this.migrationContext.GoogleCloudPlatform && !this.migrationContext.AzureMySQL {
		if impliedKey, err := mysql.GetInstanceKey(this.db); err != nil {
			return err
		} else {
			this.connectionConfig.ImpliedKey = impliedKey
		}
	}
	if err := this.readTableColumns(); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Applier initiated on %+v, version %+v", this.connectionConfig.ImpliedKey, this.migrationContext.ApplierMySQLVersion)
	return nil
}

func (this *Applier) prepareQueries() (err error) {
	if this.dmlDeleteQueryBuilder, err = sql.NewDMLDeleteQueryBuilder(
		this.migrationContext.DatabaseName,
		this.migrationContext.GetGhostTableName(),
		this.migrationContext.OriginalTableColumns,
		&this.migrationContext.UniqueKey.Columns,
	); err != nil {
		return err
	}
	if this.dmlInsertQueryBuilder, err = sql.NewDMLInsertQueryBuilder(
		this.migrationContext.DatabaseName,
		this.migrationContext.GetGhostTableName(),
		this.migrationContext.OriginalTableColumns,
		this.migrationContext.SharedColumns,
		this.migrationContext.MappedSharedColumns,
	); err != nil {
		return err
	}
	if this.dmlUpdateQueryBuilder, err = sql.NewDMLUpdateQueryBuilder(
		this.migrationContext.DatabaseName,
		this.migrationContext.GetGhostTableName(),
		this.migrationContext.OriginalTableColumns,
		this.migrationContext.SharedColumns,
		this.migrationContext.MappedSharedColumns,
		&this.migrationContext.UniqueKey.Columns,
	); err != nil {
		return err
	}
	if this.migrationContext.Checkpoint {
		if this.checkpointInsertQueryBuilder, err = sql.NewCheckpointQueryBuilder(
			this.migrationContext.DatabaseName,
			this.migrationContext.GetCheckpointTableName(),
			&this.migrationContext.UniqueKey.Columns,
		); err != nil {
			return err
		}
	}
	return nil
}

// validateAndReadGlobalVariables potentially reads server global variables, such as the time_zone and wait_timeout.
func (this *Applier) validateAndReadGlobalVariables() error {
	query := `select /* gh-ost */ @@global.time_zone, @@global.wait_timeout`
	if err := this.db.QueryRow(query).Scan(
		&this.migrationContext.ApplierTimeZone,
		&this.migrationContext.ApplierWaitTimeout,
	); err != nil {
		return err
	}

	this.migrationContext.Log.Infof("will use time_zone='%s' on applier", this.migrationContext.ApplierTimeZone)
	return nil
}

// generateSqlModeQuery return a `sql_mode = ...` query, to be wrapped with a `set session` or `set global`,
// based on gh-ost configuration:
// - User may skip strict mode
// - User may allow zero dats or zero in dates
func (this *Applier) generateSqlModeQuery() string {
	sqlModeAddendum := []string{`NO_AUTO_VALUE_ON_ZERO`}
	if !this.migrationContext.SkipStrictMode {
		sqlModeAddendum = append(sqlModeAddendum, `STRICT_ALL_TABLES`)
	}
	sqlModeQuery := fmt.Sprintf("CONCAT(@@session.sql_mode, ',%s')", strings.Join(sqlModeAddendum, ","))
	if this.migrationContext.AllowZeroInDate {
		sqlModeQuery = fmt.Sprintf("REPLACE(REPLACE(%s, 'NO_ZERO_IN_DATE', ''), 'NO_ZERO_DATE', '')", sqlModeQuery)
	}

	return fmt.Sprintf("sql_mode = %s", sqlModeQuery)
}

// generateInstantDDLQuery returns the SQL for this ALTER operation
// with an INSTANT assertion (requires MySQL 8.0+)
func (this *Applier) generateInstantDDLQuery() string {
	return fmt.Sprintf(`ALTER /* gh-ost */ TABLE %s.%s %s, ALGORITHM=INSTANT`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		this.migrationContext.AlterStatementOptions,
	)
}

// readTableColumns reads table columns on applier
func (this *Applier) readTableColumns() (err error) {
	this.migrationContext.Log.Infof("Examining table structure on applier")
	this.migrationContext.OriginalTableColumnsOnApplier, _, err = mysql.GetTableColumns(this.db, this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName)
	if err != nil {
		return err
	}
	return nil
}

// showTableStatus returns the output of `show table status like '...'` command
func (this *Applier) showTableStatus(tableName string) (rowMap sqlutils.RowMap) {
	query := fmt.Sprintf(`show /* gh-ost */ table status from %s like '%s'`, sql.EscapeName(this.migrationContext.DatabaseName), tableName)
	sqlutils.QueryRowsMap(this.db, query, func(m sqlutils.RowMap) error {
		rowMap = m
		return nil
	})
	return rowMap
}

// tableExists checks if a given table exists in database
func (this *Applier) tableExists(tableName string) (tableFound bool) {
	m := this.showTableStatus(tableName)
	return (m != nil)
}

// ValidateOrDropExistingTables verifies ghost and changelog tables do not exist,
// or attempts to drop them if instructed to.
func (this *Applier) ValidateOrDropExistingTables() error {
	if this.migrationContext.InitiallyDropGhostTable {
		if err := this.DropGhostTable(); err != nil {
			return err
		}
	}
	if this.tableExists(this.migrationContext.GetGhostTableName()) {
		return fmt.Errorf("Table %s already exists. Panicking. Use --initially-drop-ghost-table to force dropping it, though I really prefer that you drop it or rename it away", sql.EscapeName(this.migrationContext.GetGhostTableName()))
	}
	if this.migrationContext.InitiallyDropOldTable {
		if err := this.DropOldTable(); err != nil {
			return err
		}
	}
	if len(this.migrationContext.GetOldTableName()) > mysql.MaxTableNameLength {
		this.migrationContext.Log.Fatalf("--timestamp-old-table defined, but resulting table name (%s) is too long (only %d characters allowed)", this.migrationContext.GetOldTableName(), mysql.MaxTableNameLength)
	}

	if this.tableExists(this.migrationContext.GetOldTableName()) {
		return fmt.Errorf("Table %s already exists. Panicking. Use --initially-drop-old-table to force dropping it, though I really prefer that you drop it or rename it away", sql.EscapeName(this.migrationContext.GetOldTableName()))
	}

	return nil
}

// AttemptInstantDDL attempts to use instant DDL (from MySQL 8.0, and earlier in Aurora and some others).
// If successful, the operation is only a meta-data change so a lot of time is saved!
// The risk of attempting to instant DDL when not supported is that a metadata lock may be acquired.
// This is minor, since gh-ost will eventually require a metadata lock anyway, but at the cut-over stage.
// Instant operations include:
// - Adding a column
// - Dropping a column
// - Dropping an index
// - Extending a VARCHAR column
// - Adding a virtual generated column
// It is not reliable to parse the `alter` statement to determine if it is instant or not.
// This is because the table might be in an older row format, or have some other incompatibility
// that is difficult to identify.
func (this *Applier) AttemptInstantDDL() error {
	query := this.generateInstantDDLQuery()
	this.migrationContext.Log.Infof("INSTANT DDL query is: %s", query)

	// Reuse cut-over-lock-timeout from regular migration process to reduce risk
	// in situations where there may be long-running transactions.
	tableLockTimeoutSeconds := this.migrationContext.CutOverLockTimeoutSeconds * 2
	this.migrationContext.Log.Infof("Setting LOCK timeout as %d seconds", tableLockTimeoutSeconds)
	lockTimeoutQuery := fmt.Sprintf(`set /* gh-ost */ session lock_wait_timeout:=%d`, tableLockTimeoutSeconds)
	if _, err := this.db.Exec(lockTimeoutQuery); err != nil {
		return err
	}
	// We don't need a trx, because for instant DDL the SQL mode doesn't matter.
	_, err := this.db.Exec(query)
	return err
}

// CreateGhostTable creates the ghost table on the applier host
func (this *Applier) CreateGhostTable() error {
	query := fmt.Sprintf(`create /* gh-ost */ table %s.%s like %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.Log.Infof("Creating ghost table %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)

	err := func() error {
		tx, err := this.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		sessionQuery := fmt.Sprintf(`SET SESSION time_zone = '%s'`, this.migrationContext.ApplierTimeZone)
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())

		if _, err := tx.Exec(sessionQuery); err != nil {
			return err
		}
		if _, err := tx.Exec(query); err != nil {
			return err
		}
		this.migrationContext.Log.Infof("Ghost table created")
		if err := tx.Commit(); err != nil {
			// Neither SET SESSION nor ALTER are really transactional, so strictly speaking
			// there's no need to commit; but let's do this the legit way anyway.
			return err
		}
		return nil
	}()

	return err
}

func (this *Applier) getTableForeignKeyDefinitions(tableName string) (fkNames []string, fkBodies []string, err error) {
	query := fmt.Sprintf("show create table %s.%s", sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(tableName))
	var dummy string
	var createStatement string
	if err := this.db.QueryRow(query).Scan(&dummy, &createStatement); err != nil {
		return nil, nil, err
	}

	lines := strings.Split(createStatement, "\n")
	re := regexp.MustCompile("CONSTRAINT\\s+['\"`]?([^'\"`]+)['\"`]?\\s+FOREIGN\\s+KEY\\s+(.*)")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimRight(line, ",")
		if matches := re.FindStringSubmatch(line); len(matches) == 3 {
			fkNames = append(fkNames, matches[1])
			fkBodies = append(fkBodies, "FOREIGN KEY "+matches[2])
		}
	}
	return fkNames, fkBodies, nil
}

func (this *Applier) ApplyForeignKeys() error {
	this.migrationContext.Log.Infof("Applying foreign keys with prefix '%s'", this.migrationContext.ForeignKeyRenamePrefix)

	ghostFKNames, _, err := this.getTableForeignKeyDefinitions(this.migrationContext.GetGhostTableName())
	if err != nil {
		return err
	}
	for _, name := range ghostFKNames {
		query := fmt.Sprintf("alter /* gh-ost */ table %s.%s drop foreign key %s",
			sql.EscapeName(this.migrationContext.DatabaseName),
			sql.EscapeName(this.migrationContext.GetGhostTableName()),
			sql.EscapeName(name))
		this.migrationContext.Log.Infof("Dropping foreign key %s from ghost table", name)
		if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
			return err
		}
	}

	origFKNames, origFKBodies, err := this.getTableForeignKeyDefinitions(this.migrationContext.OriginalTableName)
	if err != nil {
		return err
	}
	for i, name := range origFKNames {
		newName := fmt.Sprintf("%s%s", this.migrationContext.ForeignKeyRenamePrefix, name)
		if len(newName) > 64 {
			return fmt.Errorf("Generate foreign key name '%s' exceeds 64 characters", newName)
		}
		query := fmt.Sprintf("alter /* gh-ost */ table %s.%s add constraint %s %s",
			sql.EscapeName(this.migrationContext.DatabaseName),
			sql.EscapeName(this.migrationContext.GetGhostTableName()),
			sql.EscapeName(newName),
			origFKBodies[i])
		this.migrationContext.Log.Infof("Adding foreign key %s to ghost table", newName)
		if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
			return err
		}
	}
	return nil
}

// AlterGhost applies `alter` statement on ghost table
func (this *Applier) AlterGhost() error {
	query := fmt.Sprintf(`alter /* gh-ost */ table %s.%s %s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		this.migrationContext.AlterStatementOptions,
	)
	this.migrationContext.Log.Infof("Altering ghost table %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)
	this.migrationContext.Log.Debugf("ALTER statement: %s", query)

	err := func() error {
		tx, err := this.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		sessionQuery := fmt.Sprintf(`SET SESSION time_zone = '%s'`, this.migrationContext.ApplierTimeZone)
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())

		if _, err := tx.Exec(sessionQuery); err != nil {
			return err
		}
		if _, err := tx.Exec(query); err != nil {
			return err
		}
		this.migrationContext.Log.Infof("Ghost table altered")
		if err := tx.Commit(); err != nil {
			// Neither SET SESSION nor ALTER are really transactional, so strictly speaking
			// there's no need to commit; but let's do this the legit way anyway.
			return err
		}
		return nil
	}()

	return err
}

// AlterGhost applies `alter` statement on ghost table
func (this *Applier) AlterGhostAutoIncrement() error {
	query := fmt.Sprintf(`alter /* gh-ost */ table %s.%s AUTO_INCREMENT=%d`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		this.migrationContext.OriginalTableAutoIncrement,
	)
	this.migrationContext.Log.Infof("Altering ghost table AUTO_INCREMENT value %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)
	this.migrationContext.Log.Debugf("AUTO_INCREMENT ALTER statement: %s", query)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Ghost table AUTO_INCREMENT altered")
	return nil
}

// CreateChangelogTable creates the changelog table on the applier host
func (this *Applier) CreateChangelogTable() error {
	if err := this.DropChangelogTable(); err != nil {
		return err
	}
	query := fmt.Sprintf(`create /* gh-ost */ table %s.%s (
			id bigint unsigned auto_increment,
			last_update timestamp not null DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			hint varchar(64) charset ascii not null,
			value varchar(4096) charset ascii not null,
			primary key(id),
			unique key hint_uidx(hint)
		) auto_increment=256 comment='%s'`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetChangelogTableName()),
		GhostChangelogTableComment,
	)
	this.migrationContext.Log.Infof("Creating changelog table %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetChangelogTableName()),
	)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Changelog table created")
	return nil
}

// Create the checkpoint table to store the chunk copy and applier state.
// There are two sets of columns with the same types as the shared unique key,
// one for IterationMinValues and one for IterationMaxValues.
func (this *Applier) CreateCheckpointTable() error {
	if err := this.DropCheckpointTable(); err != nil {
		return err
	}
	colDefs := []string{
		"`gh_ost_chk_id` bigint auto_increment primary key",
		"`gh_ost_chk_timestamp` bigint",
		"`gh_ost_chk_coords` text charset ascii",
		"`gh_ost_chk_iteration` bigint",
		"`gh_ost_rows_copied` bigint",
		"`gh_ost_dml_applied` bigint",
		"`gh_ost_is_cutover` tinyint(1) DEFAULT '0'",
	}
	for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
		if col.MySQLType == "" {
			return fmt.Errorf("CreateCheckpoinTable: column %s has no type information. applyColumnTypes must be called", sql.EscapeName(col.Name))
		}
		minColName := sql.TruncateColumnName(col.Name, sql.MaxColumnNameLength-4) + "_min"
		colDef := fmt.Sprintf("%s %s", sql.EscapeName(minColName), col.MySQLType)
		colDefs = append(colDefs, colDef)
	}

	for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
		maxColName := sql.TruncateColumnName(col.Name, sql.MaxColumnNameLength-4) + "_max"
		colDef := fmt.Sprintf("%s %s", sql.EscapeName(maxColName), col.MySQLType)
		colDefs = append(colDefs, colDef)
	}

	query := fmt.Sprintf("create /* gh-ost */ table %s.%s (\n %s\n)",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetCheckpointTableName()),
		strings.Join(colDefs, ",\n "),
	)
	this.migrationContext.Log.Infof("Created checkpoint table")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	return nil
}

// dropTable drops a given table on the applied host
func (this *Applier) dropTable(tableName string) error {
	query := fmt.Sprintf(`drop /* gh-ost */ table if exists %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
	)
	this.migrationContext.Log.Infof("Dropping table %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
	)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Table dropped")
	return nil
}

func (this *Applier) StateMetadataLockInstrument() error {
	// Check if performance_schema is enabled first
	var performanceSchemaValue string
	query := `SHOW VARIABLES LIKE 'performance_schema'`
	if err := this.db.QueryRow(query).Scan(new(string), &performanceSchemaValue); err != nil {
		return this.migrationContext.Log.Errorf("Unable to determine performance_schema status: %s", err)
	}

	if !strings.EqualFold(performanceSchemaValue, "ON") {
		this.migrationContext.Log.Warningf("performance_schema is disabled (value: %s)", performanceSchemaValue)
		if this.migrationContext.AllowDisabledMetadataLockInstruments {
			this.migrationContext.Log.Warningf("AllowDisabledMetadataLockInstruments is enabled, proceeding without metadata lock instrumentation")
			this.migrationContext.IsOpenMetadataLockInstruments = false
			return nil
		}
		return this.migrationContext.Log.Errorf("performance_schema is disabled. Please enable it or use --allow-disabled-metadata-lock-instruments to proceed without metadata lock checking")
	}

	query = `select /*+ MAX_EXECUTION_TIME(300) */ ENABLED, TIMED from performance_schema.setup_instruments WHERE NAME = 'wait/lock/metadata/sql/mdl'`
	var enabled, timed string
	if err := this.db.QueryRow(query).Scan(&enabled, &timed); err != nil {
		return this.migrationContext.Log.Errorf("query performance_schema.setup_instruments with name wait/lock/metadata/sql/mdl error: %s", err)
	}
	if strings.EqualFold(enabled, "YES") && strings.EqualFold(timed, "YES") {
		this.migrationContext.IsOpenMetadataLockInstruments = true
		return nil
	}
	if !this.migrationContext.AllowSetupMetadataLockInstruments {
		return nil
	}
	this.migrationContext.Log.Infof("instrument wait/lock/metadata/sql/mdl state: enabled %s, timed %s", enabled, timed)
	if _, err := this.db.Exec(`UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME = 'wait/lock/metadata/sql/mdl'`); err != nil {
		return this.migrationContext.Log.Errorf("enable instrument wait/lock/metadata/sql/mdl error: %s", err)
	}
	this.migrationContext.IsOpenMetadataLockInstruments = true
	this.migrationContext.Log.Infof("instrument wait/lock/metadata/sql/mdl enabled")
	return nil
}

// dropTriggers drop the triggers on the applied host
func (this *Applier) DropTriggersFromGhost() error {
	if len(this.migrationContext.Triggers) > 0 {
		for _, trigger := range this.migrationContext.Triggers {
			triggerName := this.migrationContext.GetGhostTriggerName(trigger.Name)
			query := fmt.Sprintf("drop trigger if exists %s", sql.EscapeName(triggerName))
			_, err := sqlutils.ExecNoPrepare(this.db, query)
			if err != nil {
				return err
			}
			this.migrationContext.Log.Infof("Trigger '%s' dropped", triggerName)
		}
	}
	return nil
}

// createTriggers creates the triggers on the applied host
func (this *Applier) createTriggers(tableName string) error {
	if len(this.migrationContext.Triggers) > 0 {
		for _, trigger := range this.migrationContext.Triggers {
			triggerName := this.migrationContext.GetGhostTriggerName(trigger.Name)
			query := fmt.Sprintf(`create /* gh-ost */ trigger %s %s %s on %s.%s for each row
		%s`,
				sql.EscapeName(triggerName),
				trigger.Timing,
				trigger.Event,
				sql.EscapeName(this.migrationContext.DatabaseName),
				sql.EscapeName(tableName),
				trigger.Statement,
			)
			this.migrationContext.Log.Infof("Createing trigger %s on %s.%s",
				sql.EscapeName(triggerName),
				sql.EscapeName(this.migrationContext.DatabaseName),
				sql.EscapeName(tableName),
			)
			if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
				return err
			}
		}
		this.migrationContext.Log.Infof("Triggers created on %s", tableName)
	}
	return nil
}

// CreateTriggers creates the original triggers on applier host
func (this *Applier) CreateTriggersOnGhost() error {
	err := this.createTriggers(this.migrationContext.GetGhostTableName())
	return err
}

// DropChangelogTable drops the changelog table on the applier host
func (this *Applier) DropChangelogTable() error {
	return this.dropTable(this.migrationContext.GetChangelogTableName())
}

// DropCheckpointTable drops the checkpoint table on applier host
func (this *Applier) DropCheckpointTable() error {
	return this.dropTable(this.migrationContext.GetCheckpointTableName())
}

// DropOldTable drops the _Old table on the applier host
func (this *Applier) DropOldTable() error {
	return this.dropTable(this.migrationContext.GetOldTableName())
}

// DropGhostTable drops the ghost table on the applier host
func (this *Applier) DropGhostTable() error {
	return this.dropTable(this.migrationContext.GetGhostTableName())
}

// WriteChangelog writes a value to the changelog table.
// It returns the hint as given, for convenience
func (this *Applier) WriteChangelog(hint, value string) (string, error) {
	explicitId := 0
	switch hint {
	case "heartbeat":
		explicitId = 1
	case "state":
		explicitId = 2
	case "throttle":
		explicitId = 3
	}
	query := fmt.Sprintf(`
		insert /* gh-ost */
		into
			%s.%s
			(id, hint, value)
		values
			(NULLIF(?, 0), ?, ?)
		on duplicate key update
			last_update=NOW(),
			value=VALUES(value)`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetChangelogTableName()),
	)
	_, err := sqlutils.ExecNoPrepare(this.db, query, explicitId, hint, value)
	return hint, err
}

func (this *Applier) WriteAndLogChangelog(hint, value string) (string, error) {
	this.WriteChangelog(hint, value)
	return this.WriteChangelog(fmt.Sprintf("%s at %d", hint, time.Now().UnixNano()), value)
}

func (this *Applier) WriteChangelogState(value string) (string, error) {
	return this.WriteAndLogChangelog("state", value)
}

// WriteCheckpoints writes a checkpoint to the _ghk table.
func (this *Applier) WriteCheckpoint(chk *Checkpoint) (int64, error) {
	var insertId int64
	uniqueKeyArgs := sqlutils.Args(chk.IterationRangeMin.AbstractValues()...)
	uniqueKeyArgs = append(uniqueKeyArgs, chk.IterationRangeMax.AbstractValues()...)
	query, uniqueKeyArgs, err := this.checkpointInsertQueryBuilder.BuildQuery(uniqueKeyArgs)
	if err != nil {
		return insertId, err
	}
	args := sqlutils.Args(chk.LastTrxCoords.String(), chk.Iteration, chk.RowsCopied, chk.DMLApplied, chk.IsCutover)
	args = append(args, uniqueKeyArgs...)
	res, err := this.db.Exec(query, args...)
	if err != nil {
		return insertId, err
	}
	return res.LastInsertId()
}

func (this *Applier) ReadLastCheckpoint() (*Checkpoint, error) {
	row := this.db.QueryRow(fmt.Sprintf(`select /* gh-ost */ * from %s.%s order by gh_ost_chk_id desc limit 1`, sql.EscapeName(this.migrationContext.DatabaseName), sql.EscapeName(this.migrationContext.GetCheckpointTableName())))
	chk := &Checkpoint{
		IterationRangeMin: sql.NewColumnValues(this.migrationContext.UniqueKey.Columns.Len()),
		IterationRangeMax: sql.NewColumnValues(this.migrationContext.UniqueKey.Columns.Len()),
	}

	var coordStr string
	var timestamp int64
	ptrs := []interface{}{&chk.Id, &timestamp, &coordStr, &chk.Iteration, &chk.RowsCopied, &chk.DMLApplied, &chk.IsCutover}
	ptrs = append(ptrs, chk.IterationRangeMin.ValuesPointers...)
	ptrs = append(ptrs, chk.IterationRangeMax.ValuesPointers...)
	err := row.Scan(ptrs...)
	if err != nil {
		if errors.Is(err, gosql.ErrNoRows) {
			return nil, ErrNoCheckpointFound
		}
		return nil, err
	}
	chk.Timestamp = time.Unix(timestamp, 0)
	if this.migrationContext.UseGTIDs {
		gtidCoords, err := mysql.NewGTIDBinlogCoordinates(coordStr)
		if err != nil {
			return nil, err
		}
		chk.LastTrxCoords = gtidCoords
	} else {
		fileCoords, err := mysql.ParseFileBinlogCoordinates(coordStr)
		if err != nil {
			return nil, err
		}
		chk.LastTrxCoords = fileCoords
	}
	return chk, nil
}

// InitiateHeartbeat creates a heartbeat cycle, writing to the changelog table.
// This is done asynchronously
func (this *Applier) InitiateHeartbeat() {
	var numSuccessiveFailures int64
	injectHeartbeat := func() error {
		if atomic.LoadInt64(&this.migrationContext.HibernateUntil) > 0 {
			return nil
		}
		if _, err := this.WriteChangelog("heartbeat", time.Now().Format(time.RFC3339Nano)); err != nil {
			numSuccessiveFailures++
			if numSuccessiveFailures > this.migrationContext.MaxRetries() {
				return this.migrationContext.Log.Errore(err)
			}
		} else {
			numSuccessiveFailures = 0
		}
		return nil
	}
	injectHeartbeat()

	ticker := time.NewTicker(time.Duration(this.migrationContext.HeartbeatIntervalMilliseconds) * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if atomic.LoadInt64(&this.finishedMigrating) > 0 {
			return
		}
		// Generally speaking, we would issue a goroutine, but I'd actually rather
		// have this block the loop rather than spam the master in the event something
		// goes wrong
		if throttle, _, reasonHint := this.migrationContext.IsThrottled(); throttle && (reasonHint == base.UserCommandThrottleReasonHint) {
			continue
		}
		if err := injectHeartbeat(); err != nil {
			this.migrationContext.PanicAbort <- fmt.Errorf("injectHeartbeat writing failed %d times, last error: %w", numSuccessiveFailures, err)
			return
		}
	}
}

// ExecuteThrottleQuery executes the `--throttle-query` and returns its results.
func (this *Applier) ExecuteThrottleQuery() (int64, error) {
	throttleQuery := this.migrationContext.GetThrottleQuery()

	if throttleQuery == "" {
		return 0, nil
	}
	var result int64
	if err := this.db.QueryRow(throttleQuery).Scan(&result); err != nil {
		return 0, this.migrationContext.Log.Errore(err)
	}
	return result, nil
}

// readMigrationMinValues returns the minimum values to be iterated on rowcopy
func (this *Applier) readMigrationMinValues(tx *gosql.Tx, uniqueKey *sql.UniqueKey) error {
	this.migrationContext.Log.Debugf("Reading migration range according to key: %s", uniqueKey.Name)
	query, err := sql.BuildUniqueKeyMinValuesPreparedQuery(this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName, uniqueKey)
	if err != nil {
		return err
	}

	rows, err := tx.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		this.migrationContext.MigrationRangeMinValues = sql.NewColumnValues(uniqueKey.Len())
		if err = rows.Scan(this.migrationContext.MigrationRangeMinValues.ValuesPointers...); err != nil {
			return err
		}
	}
	this.migrationContext.Log.Infof("Migration min values: [%s]", this.migrationContext.MigrationRangeMinValues)

	return rows.Err()
}

// readMigrationMaxValues returns the maximum values to be iterated on rowcopy
func (this *Applier) readMigrationMaxValues(tx *gosql.Tx, uniqueKey *sql.UniqueKey) error {
	this.migrationContext.Log.Debugf("Reading migration range according to key: %s", uniqueKey.Name)
	query, err := sql.BuildUniqueKeyMaxValuesPreparedQuery(this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName, uniqueKey)
	if err != nil {
		return err
	}

	rows, err := tx.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		this.migrationContext.MigrationRangeMaxValues = sql.NewColumnValues(uniqueKey.Len())
		if err = rows.Scan(this.migrationContext.MigrationRangeMaxValues.ValuesPointers...); err != nil {
			return err
		}
	}
	this.migrationContext.Log.Infof("Migration max values: [%s]", this.migrationContext.MigrationRangeMaxValues)

	return rows.Err()
}

// ReadMigrationRangeValues reads min/max values that will be used for rowcopy.
// Before read min/max, write a changelog state into the ghc table to avoid lost data in mysql two-phase commit.
/*
Detail description of the lost data in mysql two-phase commit issue by @Fanduzi:
	When using semi-sync and setting rpl_semi_sync_master_wait_point=AFTER_SYNC,
	if an INSERT statement is being committed but blocks due to an unmet ack count,
	the data inserted by the transaction is not visible to ReadMigrationRangeValues,
	so the copy of the existing data in the table does not include the new row inserted by the transaction.
	However, the binlog event for the transaction is already written to the binlog,
	so the addDMLEventsListener only captures the binlog event after the transaction,
	and thus the transaction's binlog event is not captured, resulting in data loss.

	If write a changelog into ghc table before ReadMigrationRangeValues, and the transaction commit blocks
	because the ack is not met, then the changelog will not be able to write, so the ReadMigrationRangeValues
	will not be run. When the changelog writes successfully, the ReadMigrationRangeValues will read the
	newly inserted data, thus Avoiding data loss due to the above problem.
*/
func (this *Applier) ReadMigrationRangeValues() error {
	if _, err := this.WriteChangelogState(string(ReadMigrationRangeValues)); err != nil {
		return err
	}

	tx, err := this.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := this.readMigrationMinValues(tx, this.migrationContext.UniqueKey); err != nil {
		return err
	}
	if err := this.readMigrationMaxValues(tx, this.migrationContext.UniqueKey); err != nil {
		return err
	}

	return tx.Commit()
}

// CalculateNextIterationRangeEndValues reads the next-iteration-range-end unique key values,
// which will be used for copying the next chunk of rows. Ir returns "false" if there is
// no further chunk to work through, i.e. we're past the last chunk and are done with
// iterating the range (and thus done with copying row chunks)
func (this *Applier) CalculateNextIterationRangeEndValues() (hasFurtherRange bool, err error) {
	this.LastIterationRangeMutex.Lock()
	if this.migrationContext.MigrationIterationRangeMinValues != nil && this.migrationContext.MigrationIterationRangeMaxValues != nil {
		this.LastIterationRangeMinValues = this.migrationContext.MigrationIterationRangeMinValues.Clone()
		this.LastIterationRangeMaxValues = this.migrationContext.MigrationIterationRangeMaxValues.Clone()
	}
	this.LastIterationRangeMutex.Unlock()

	this.migrationContext.MigrationIterationRangeMinValues = this.migrationContext.MigrationIterationRangeMaxValues
	if this.migrationContext.MigrationIterationRangeMinValues == nil {
		this.migrationContext.MigrationIterationRangeMinValues = this.migrationContext.MigrationRangeMinValues
	}
	for i := 0; i < 2; i++ {
		buildFunc := sql.BuildUniqueKeyRangeEndPreparedQueryViaOffset
		if i == 1 {
			buildFunc = sql.BuildUniqueKeyRangeEndPreparedQueryViaTemptable
		}
		query, explodedArgs, err := buildFunc(
			this.migrationContext.DatabaseName,
			this.migrationContext.OriginalTableName,
			&this.migrationContext.UniqueKey.Columns,
			this.migrationContext.MigrationIterationRangeMinValues.AbstractValues(),
			this.migrationContext.MigrationRangeMaxValues.AbstractValues(),
			atomic.LoadInt64(&this.migrationContext.ChunkSize),
			this.migrationContext.GetIteration() == 0,
			fmt.Sprintf("iteration:%d", this.migrationContext.GetIteration()),
		)
		if err != nil {
			return hasFurtherRange, err
		}

		rows, err := this.db.Query(query, explodedArgs...)
		if err != nil {
			return hasFurtherRange, err
		}
		defer rows.Close()

		iterationRangeMaxValues := sql.NewColumnValues(this.migrationContext.UniqueKey.Len())
		for rows.Next() {
			if err = rows.Scan(iterationRangeMaxValues.ValuesPointers...); err != nil {
				return hasFurtherRange, err
			}
			hasFurtherRange = true
		}
		if err = rows.Err(); err != nil {
			return hasFurtherRange, err
		}
		if hasFurtherRange {
			this.migrationContext.MigrationIterationRangeMaxValues = iterationRangeMaxValues
			return hasFurtherRange, nil
		}
	}
	this.migrationContext.Log.Debugf("Iteration complete: no further range to iterate")
	return hasFurtherRange, nil
}

// ApplyIterationInsertQuery issues a chunk-INSERT query on the ghost table. It is where
// data actually gets copied from original table.
func (this *Applier) ApplyIterationInsertQuery() (chunkSize int64, rowsAffected int64, duration time.Duration, err error) {
	startTime := time.Now()
	chunkSize = atomic.LoadInt64(&this.migrationContext.ChunkSize)

	query, explodedArgs, err := sql.BuildRangeInsertPreparedQuery(
		this.migrationContext.DatabaseName,
		this.migrationContext.OriginalTableName,
		this.migrationContext.GetGhostTableName(),
		this.migrationContext.SharedColumns.Names(),
		this.migrationContext.MappedSharedColumns.Names(),
		this.migrationContext.UniqueKey.Name,
		&this.migrationContext.UniqueKey.Columns,
		this.migrationContext.MigrationIterationRangeMinValues.AbstractValues(),
		this.migrationContext.MigrationIterationRangeMaxValues.AbstractValues(),
		this.migrationContext.GetIteration() == 0,
		this.migrationContext.IsTransactionalTable(),
		// TODO: Don't hardcode this
		strings.HasPrefix(this.migrationContext.ApplierMySQLVersion, "8."),
	)
	if err != nil {
		return chunkSize, rowsAffected, duration, err
	}

	sqlResult, err := func() (gosql.Result, error) {
		tx, err := this.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()

		sessionQuery := fmt.Sprintf(`SET SESSION time_zone = '%s'`, this.migrationContext.ApplierTimeZone)
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())

		if _, err := tx.Exec(sessionQuery); err != nil {
			return nil, err
		}
		result, err := tx.Exec(query, explodedArgs...)
		if err != nil {
			return nil, err
		}

		if this.migrationContext.PanicOnWarnings {
			//nolint:execinquery
			rows, err := tx.Query("SHOW WARNINGS")
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			if err = rows.Err(); err != nil {
				return nil, err
			}

			var sqlWarnings []string
			for rows.Next() {
				var level, message string
				var code int
				if err := rows.Scan(&level, &code, &message); err != nil {
					this.migrationContext.Log.Warningf("Failed to read SHOW WARNINGS row")
					continue
				}
				// Duplicate warnings are formatted differently across mysql versions, hence the optional table name prefix
				migrationUniqueKeyExpression := fmt.Sprintf("for key '(%s\\.)?%s'", this.migrationContext.GetGhostTableName(), this.migrationContext.UniqueKey.NameInGhostTable)
				matched, _ := regexp.MatchString(migrationUniqueKeyExpression, message)
				if strings.Contains(message, "Duplicate entry") && matched {
					continue
				}
				sqlWarnings = append(sqlWarnings, fmt.Sprintf("%s: %s (%d)", level, message, code))
			}
			this.migrationContext.MigrationLastInsertSQLWarnings = sqlWarnings
		}

		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return result, nil
	}()

	if err != nil {
		return chunkSize, rowsAffected, duration, err
	}
	rowsAffected, _ = sqlResult.RowsAffected()
	duration = time.Since(startTime)
	this.migrationContext.Log.Debugf(
		"Issued INSERT on range: [%s]..[%s]; iteration: %d; chunk-size: %d",
		this.migrationContext.MigrationIterationRangeMinValues,
		this.migrationContext.MigrationIterationRangeMaxValues,
		this.migrationContext.GetIteration(),
		chunkSize)
	return chunkSize, rowsAffected, duration, nil
}

// LockOriginalTable places a write lock on the original table
func (this *Applier) LockOriginalTable() error {
	query := fmt.Sprintf(`lock /* gh-ost */ tables %s.%s write`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.Log.Infof("Locking %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.LockTablesStartTime = time.Now()
	if _, err := sqlutils.ExecNoPrepare(this.singletonDB, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Table locked")
	return nil
}

// UnlockTables makes tea. No wait, it unlocks tables.
func (this *Applier) UnlockTables() error {
	query := `unlock /* gh-ost */ tables`
	this.migrationContext.Log.Infof("Unlocking tables")
	if _, err := sqlutils.ExecNoPrepare(this.singletonDB, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Tables unlocked")
	return nil
}

// SwapTablesQuickAndBumpy issues a two-step swap table operation:
// - rename original table to _old
// - rename ghost table to original
// There is a point in time in between where the table does not exist.
func (this *Applier) SwapTablesQuickAndBumpy() error {
	query := fmt.Sprintf(`alter /* gh-ost */ table %s.%s rename %s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	this.migrationContext.Log.Infof("Renaming original table")
	this.migrationContext.RenameTablesStartTime = time.Now()
	if _, err := sqlutils.ExecNoPrepare(this.singletonDB, query); err != nil {
		return err
	}
	query = fmt.Sprintf(`alter /* gh-ost */ table %s.%s rename %s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.Log.Infof("Renaming ghost table")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.RenameTablesEndTime = time.Now()

	this.migrationContext.Log.Infof("Tables renamed")
	return nil
}

// RenameTablesRollback renames back both table: original back to ghost,
// _old back to original. This is used by `--test-on-replica`
func (this *Applier) RenameTablesRollback() (renameError error) {
	// Restoring tables to original names.
	// We prefer the single, atomic operation:
	query := fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s, %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.Log.Infof("Renaming back both tables")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err == nil {
		return nil
	}
	// But, if for some reason the above was impossible to do, we rename one by one.
	query = fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)
	this.migrationContext.Log.Infof("Renaming back to ghost table")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		renameError = err
	}
	query = fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.Log.Infof("Renaming back to original table")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		renameError = err
	}
	return this.migrationContext.Log.Errore(renameError)
}

// StopSlaveIOThread is applicable with --test-on-replica; it stops the IO thread, duh.
// We need to keep the SQL thread active so as to complete processing received events,
// and have them written to the binary log, so that we can then read them via streamer.
func (this *Applier) StopSlaveIOThread() error {
	replicaTerm := mysql.ReplicaTermFor(this.migrationContext.ApplierMySQLVersion, `slave`)
	query := fmt.Sprintf("stop /* gh-ost */ %s io_thread", replicaTerm)
	this.migrationContext.Log.Infof("Stopping replication IO thread")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Replication IO thread stopped")
	return nil
}

// StartSlaveIOThread is applicable with --test-on-replica
func (this *Applier) StartSlaveIOThread() error {
	replicaTerm := mysql.ReplicaTermFor(this.migrationContext.ApplierMySQLVersion, `slave`)
	query := fmt.Sprintf("start /* gh-ost */ %s io_thread", replicaTerm)
	this.migrationContext.Log.Infof("Starting replication IO thread")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Replication IO thread started")
	return nil
}

// StopSlaveSQLThread is applicable with --test-on-replica
func (this *Applier) StopSlaveSQLThread() error {
	replicaTerm := mysql.ReplicaTermFor(this.migrationContext.ApplierMySQLVersion, `slave`)
	query := fmt.Sprintf("stop /* gh-ost */ %s sql_thread", replicaTerm)
	this.migrationContext.Log.Infof("Verifying SQL thread is stopped")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("SQL thread stopped")
	return nil
}

// StartSlaveSQLThread is applicable with --test-on-replica
func (this *Applier) StartSlaveSQLThread() error {
	replicaTerm := mysql.ReplicaTermFor(this.migrationContext.ApplierMySQLVersion, `slave`)
	query := fmt.Sprintf("start /* gh-ost */ %s sql_thread", replicaTerm)
	this.migrationContext.Log.Infof("Verifying SQL thread is running")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("SQL thread started")
	return nil
}

// StopReplication is used by `--test-on-replica` and stops replication.
func (this *Applier) StopReplication() error {
	if err := this.StopSlaveIOThread(); err != nil {
		return err
	}
	if err := this.StopSlaveSQLThread(); err != nil {
		return err
	}

	readBinlogCoordinates, executeBinlogCoordinates, err := mysql.GetReplicationBinlogCoordinates(this.migrationContext.ApplierMySQLVersion, this.db, this.migrationContext.UseGTIDs)
	if err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Replication IO thread at %+v. SQL thread is at %+v", readBinlogCoordinates, executeBinlogCoordinates)
	return nil
}

// StartReplication is used by `--test-on-replica` on cut-over failure
func (this *Applier) StartReplication() error {
	if err := this.StartSlaveIOThread(); err != nil {
		return err
	}
	if err := this.StartSlaveSQLThread(); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Replication started")
	return nil
}

// GetSessionLockName returns a name for the special hint session voluntary lock
func (this *Applier) GetSessionLockName(sessionId int64) string {
	return fmt.Sprintf("gh-ost.%d.lock", sessionId)
}

// ExpectUsedLock expects the special hint voluntary lock to exist on given session
func (this *Applier) ExpectUsedLock(sessionId int64) error {
	var result int64
	query := `select /* gh-ost */ is_used_lock(?)`
	lockName := this.GetSessionLockName(sessionId)
	this.migrationContext.Log.Infof("Checking session lock: %s", lockName)
	if err := this.db.QueryRow(query, lockName).Scan(&result); err != nil || result != sessionId {
		return fmt.Errorf("Session lock %s expected to be found but wasn't", lockName)
	}
	return nil
}

// ExpectProcess expects a process to show up in `SHOW PROCESSLIST` that has given characteristics
func (this *Applier) ExpectProcess(sessionId int64, stateHint, infoHint string) error {
	found := false
	query := `
		select /* gh-ost */ id
		from
			information_schema.processlist
		where
			id != connection_id()
			and ? in (0, id)
			and state like concat('%', ?, '%')
			and info like concat('%', ?, '%')`
	err := sqlutils.QueryRowsMap(this.db, query, func(m sqlutils.RowMap) error {
		found = true
		return nil
	}, sessionId, stateHint, infoHint)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Cannot find process. Hints: %s, %s", stateHint, infoHint)
	}
	return nil
}

// DropAtomicCutOverSentryTableIfExists checks if the "old" table name
// happens to be a cut-over magic table; if so, it drops it.
func (this *Applier) DropAtomicCutOverSentryTableIfExists() error {
	this.migrationContext.Log.Infof("Looking for magic cut-over table")
	tableName := this.migrationContext.GetOldTableName()
	rowMap := this.showTableStatus(tableName)
	if rowMap == nil {
		// Table does not exist
		return nil
	}
	if rowMap["Comment"].String != atomicCutOverMagicHint {
		return fmt.Errorf("Expected magic comment on %s, did not find it", tableName)
	}
	this.migrationContext.Log.Infof("Dropping magic cut-over table")
	return this.dropTable(tableName)
}

// CreateAtomicCutOverSentryTable
func (this *Applier) CreateAtomicCutOverSentryTable() error {
	if err := this.DropAtomicCutOverSentryTableIfExists(); err != nil {
		return err
	}
	tableName := this.migrationContext.GetOldTableName()

	query := fmt.Sprintf(`
		create /* gh-ost */ table %s.%s (
			id int auto_increment primary key
		) engine=%s comment='%s'`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
		this.migrationContext.TableEngine,
		atomicCutOverMagicHint,
	)
	this.migrationContext.Log.Infof("Creating magic cut-over table %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
	)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	this.migrationContext.Log.Infof("Magic cut-over table created")

	return nil
}

// InitAtomicCutOverWaitTimeout sets the cut-over session wait_timeout in order to reduce the
// time an unresponsive (but still connected) gh-ost process can hold the cut-over lock.
func (this *Applier) InitAtomicCutOverWaitTimeout(tx *gosql.Tx) error {
	cutOverWaitTimeoutSeconds := this.migrationContext.CutOverLockTimeoutSeconds * 3
	this.migrationContext.Log.Infof("Setting cut-over idle timeout as %d seconds", cutOverWaitTimeoutSeconds)
	query := fmt.Sprintf(`set /* gh-ost */ session wait_timeout:=%d`, cutOverWaitTimeoutSeconds)
	_, err := tx.Exec(query)
	return err
}

// RevertAtomicCutOverWaitTimeout restores the original wait_timeout for the applier session post-cut-over.
func (this *Applier) RevertAtomicCutOverWaitTimeout() {
	this.migrationContext.Log.Infof("Reverting cut-over idle timeout to %d seconds", this.migrationContext.ApplierWaitTimeout)
	query := fmt.Sprintf(`set /* gh-ost */ session wait_timeout:=%d`, this.migrationContext.ApplierWaitTimeout)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		this.migrationContext.Log.Errorf("Failed to restore applier wait_timeout to %d seconds: %v",
			this.migrationContext.ApplierWaitTimeout, err,
		)
	}
}

// AtomicCutOverMagicLock
func (this *Applier) AtomicCutOverMagicLock(sessionIdChan chan int64, tableLocked chan<- error, okToUnlockTable <-chan bool, tableUnlocked chan<- error, renameLockSessionId *int64) error {
	tx, err := this.db.Begin()
	if err != nil {
		tableLocked <- err
		return err
	}
	defer func() {
		sessionIdChan <- -1
		tableLocked <- fmt.Errorf("Unexpected error in AtomicCutOverMagicLock(), injected to release blocking channel reads")
		tableUnlocked <- fmt.Errorf("Unexpected error in AtomicCutOverMagicLock(), injected to release blocking channel reads")
		tx.Rollback()
		this.DropAtomicCutOverSentryTableIfExists()
	}()

	var sessionId int64
	if err := tx.QueryRow(`select /* gh-ost */ connection_id()`).Scan(&sessionId); err != nil {
		tableLocked <- err
		return err
	}
	sessionIdChan <- sessionId

	lockResult := 0
	query := `select /* gh-ost */ get_lock(?, 0)`
	lockName := this.GetSessionLockName(sessionId)
	this.migrationContext.Log.Infof("Grabbing voluntary lock: %s", lockName)
	if err := tx.QueryRow(query, lockName).Scan(&lockResult); err != nil || lockResult != 1 {
		err := fmt.Errorf("Unable to acquire lock %s", lockName)
		tableLocked <- err
		return err
	}

	tableLockTimeoutSeconds := this.migrationContext.CutOverLockTimeoutSeconds * 2
	this.migrationContext.Log.Infof("Setting LOCK timeout as %d seconds", tableLockTimeoutSeconds)
	query = fmt.Sprintf(`set /* gh-ost */ session lock_wait_timeout:=%d`, tableLockTimeoutSeconds)
	if _, err := tx.Exec(query); err != nil {
		tableLocked <- err
		return err
	}

	if err := this.CreateAtomicCutOverSentryTable(); err != nil {
		tableLocked <- err
		return err
	}

	if err := this.InitAtomicCutOverWaitTimeout(tx); err != nil {
		tableLocked <- err
		return err
	}
	defer this.RevertAtomicCutOverWaitTimeout()

	query = fmt.Sprintf(`lock /* gh-ost */ tables %s.%s write, %s.%s write`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	this.migrationContext.Log.Infof("Locking %s.%s, %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	this.migrationContext.LockTablesStartTime = time.Now()
	if _, err := tx.Exec(query); err != nil {
		tableLocked <- err
		return err
	}
	this.migrationContext.Log.Infof("Tables locked")
	tableLocked <- nil // No error.

	// From this point on, we are committed to UNLOCK TABLES. No matter what happens,
	// the UNLOCK must execute (or, alternatively, this connection dies, which gets the same impact)

	// The cut-over phase will proceed to apply remaining backlog onto ghost table,
	// and issue RENAME. We wait here until told to proceed.
	<-okToUnlockTable
	this.migrationContext.Log.Infof("Will now proceed to drop magic table and unlock tables")

	// The magic table is here because we locked it. And we are the only ones allowed to drop it.
	// And in fact, we will:
	this.migrationContext.Log.Infof("Dropping magic cut-over table")
	query = fmt.Sprintf(`drop /* gh-ost */ table if exists %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)

	if _, err := tx.Exec(query); err != nil {
		this.migrationContext.Log.Errore(err)
		// We DO NOT return here because we must `UNLOCK TABLES`!
	}

	this.migrationContext.Log.Infof("Session renameLockSessionId is %+v", *renameLockSessionId)
	// Checking the lock is held by rename session
	if *renameLockSessionId > 0 && this.migrationContext.IsOpenMetadataLockInstruments {
		sleepDuration := time.Duration(10*this.migrationContext.CutOverLockTimeoutSeconds) * time.Millisecond
		for i := 1; i <= 100; i++ {
			err := this.ExpectMetadataLock(*renameLockSessionId)
			if err == nil {
				this.migrationContext.Log.Infof("Rename session is pending lock on the origin table !")
				break
			} else {
				time.Sleep(sleepDuration)
			}
		}
	}
	// Tables still locked
	this.migrationContext.Log.Infof("Releasing lock from %s.%s, %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	query = `unlock /* gh-ost */ tables`
	if _, err := tx.Exec(query); err != nil {
		tableUnlocked <- err
		return this.migrationContext.Log.Errore(err)
	}
	this.migrationContext.Log.Infof("Tables unlocked")
	tableUnlocked <- nil
	return nil
}

// AtomicCutoverRename
func (this *Applier) AtomicCutoverRename(sessionIdChan chan int64, tablesRenamed chan<- error) error {
	tx, err := this.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		tx.Rollback()
		sessionIdChan <- -1
		tablesRenamed <- fmt.Errorf("Unexpected error in AtomicCutoverRename(), injected to release blocking channel reads")
	}()
	var sessionId int64
	if err := tx.QueryRow(`select /* gh-ost */ connection_id()`).Scan(&sessionId); err != nil {
		return err
	}
	sessionIdChan <- sessionId

	this.migrationContext.Log.Infof("Setting RENAME timeout as %d seconds", this.migrationContext.CutOverLockTimeoutSeconds)
	query := fmt.Sprintf(`set /* gh-ost */ session lock_wait_timeout:=%d`, this.migrationContext.CutOverLockTimeoutSeconds)
	if _, err := tx.Exec(query); err != nil {
		return err
	}

	query = fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s, %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	this.migrationContext.Log.Infof("Issuing and expecting this to block: %s", query)
	if _, err := tx.Exec(query); err != nil {
		tablesRenamed <- err
		return this.migrationContext.Log.Errore(err)
	}
	tablesRenamed <- nil
	this.migrationContext.Log.Infof("Tables renamed")
	return nil
}

func (this *Applier) ShowStatusVariable(variableName string) (result int64, err error) {
	query := fmt.Sprintf(`show /* gh-ost */ global status like '%s'`, variableName)
	if err := this.db.QueryRow(query).Scan(&variableName, &result); err != nil {
		return 0, err
	}
	return result, nil
}

// updateModifiesUniqueKeyColumns checks whether a UPDATE DML event actually
// modifies values of the migration's unique key (the iterated key). This will call
// for special handling.
func (this *Applier) updateModifiesUniqueKeyColumns(dmlEvent *binlog.BinlogDMLEvent) (modifiedColumn string, isModified bool) {
	for _, column := range this.migrationContext.UniqueKey.Columns.Columns() {
		tableOrdinal := this.migrationContext.OriginalTableColumns.Ordinals[column.Name]
		whereColumnValue := dmlEvent.WhereColumnValues.AbstractValues()[tableOrdinal]
		newColumnValue := dmlEvent.NewColumnValues.AbstractValues()[tableOrdinal]

		if !reflect.DeepEqual(whereColumnValue, newColumnValue) {
			return column.Name, true
		}
	}
	return "", false
}

// buildDMLEventQuery creates a query to operate on the ghost table, based on an intercepted binlog
// event entry on the original table.
func (this *Applier) buildDMLEventQuery(dmlEvent *binlog.BinlogDMLEvent) []*dmlBuildResult {
	switch dmlEvent.DML {
	case binlog.DeleteDML:
		{
			query, uniqueKeyArgs, err := this.dmlDeleteQueryBuilder.BuildQuery(dmlEvent.WhereColumnValues.AbstractValues())
			return []*dmlBuildResult{newDmlBuildResult(query, uniqueKeyArgs, -1, err)}
		}
	case binlog.InsertDML:
		{
			query, sharedArgs, err := this.dmlInsertQueryBuilder.BuildQuery(dmlEvent.NewColumnValues.AbstractValues())
			return []*dmlBuildResult{newDmlBuildResult(query, sharedArgs, 1, err)}
		}
	case binlog.UpdateDML:
		{
			if _, isModified := this.updateModifiesUniqueKeyColumns(dmlEvent); isModified {
				results := make([]*dmlBuildResult, 0, 2)
				dmlEvent.DML = binlog.DeleteDML
				results = append(results, this.buildDMLEventQuery(dmlEvent)...)
				dmlEvent.DML = binlog.InsertDML
				results = append(results, this.buildDMLEventQuery(dmlEvent)...)
				return results
			}
			query, sharedArgs, uniqueKeyArgs, err := this.dmlUpdateQueryBuilder.BuildQuery(dmlEvent.NewColumnValues.AbstractValues(), dmlEvent.WhereColumnValues.AbstractValues())
			args := sqlutils.Args()
			args = append(args, sharedArgs...)
			args = append(args, uniqueKeyArgs...)
			return []*dmlBuildResult{newDmlBuildResult(query, args, 0, err)}
		}
	}
	return []*dmlBuildResult{newDmlBuildResultError(fmt.Errorf("Unknown dml event type: %+v", dmlEvent.DML))}
}

// ApplyDMLEventQueries applies multiple DML queries onto the _ghost_ table
// Maintains binlog order: builds multi-statement query with all events in sequence
func (this *Applier) ApplyDMLEventQueries(dmlEvents []*binlog.BinlogDMLEvent) error {
	var totalDelta int64
	var err error
	ctx := context.Background()

	// Try batch execution first (normal path - no tracked PKs)
	hasTrackedPKs := false
	for _, dmlEvent := range dmlEvents {
		pkKey := this.serializePKValues(dmlEvent)
		if this.isFKTrackedPK(pkKey) && dmlEvent.DML != binlog.DeleteDML {
			hasTrackedPKs = true
			break
		}
	}

	if !hasTrackedPKs {
		// Fast path: no tracked PKs, use normal batch execution
		err = this.executeDMLEvents(ctx, dmlEvents, &totalDelta)
	} else {
		// Has tracked PKs: use ordered multi-statement to maintain binlog sequence
		err = this.applyEventsOrderedMultiStatement(ctx, dmlEvents, &totalDelta)
	}

	if err != nil {
		if isForeignKeyError(err) && this.migrationContext.FKFallbackEnabled {
			// FK error - fallback to ordered multi-statement with FK handling
			this.migrationContext.Log.Warningf("Batch DML failed with FK error, falling back to ordered multi-statement: %v", err)
			totalDelta = 0 // Reset delta before fallback
			err = this.applyEventsWithFKFallback(ctx, dmlEvents, &totalDelta)
			if err != nil {
				return this.migrationContext.Log.Errore(err)
			}
		} else {
			return this.migrationContext.Log.Errore(err)
		}
	}

	atomic.AddInt64(&this.migrationContext.TotalDMLEventsApplied, int64(len(dmlEvents)))
	if this.migrationContext.CountTableRows {
		atomic.AddInt64(&this.migrationContext.RowsDeltaEstimate, totalDelta)
	}
	this.migrationContext.Log.Debugf("ApplyDMLEventQueries() applied %d events", len(dmlEvents))
	return nil
}

// applyEventsOrderedMultiStatement builds a multi-statement query maintaining binlog order
// Tracked PKs use INSERT...SELECT FROM original, others use normal DML
func (this *Applier) applyEventsOrderedMultiStatement(ctx context.Context, dmlEvents []*binlog.BinlogDMLEvent, totalDelta *int64) error {
	conn, err := this.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Set session variables
	sessionQuery := "SET /* gh-ost */ SESSION time_zone = '+00:00'"
	sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())
	if this.migrationContext.AcceptHighRiskOrphanedForeignKeys {
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, "foreign_key_checks = 0")
	}
	if _, err := conn.ExecContext(ctx, sessionQuery); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}

	// Build multi-statement query maintaining order
	var queryBuilder strings.Builder
	var allArgs []interface{}
	buildResults := make([]*dmlBuildResult, 0, len(dmlEvents))

	for _, dmlEvent := range dmlEvents {
		pkKey := this.serializePKValues(dmlEvent)
		isTracked := this.isFKTrackedPK(pkKey) && dmlEvent.DML != binlog.DeleteDML

		if isTracked {
			// Tracked PK: use INSERT...SELECT FROM original table
			query, args := this.buildInsertSelectFromOriginal(dmlEvent)
			queryBuilder.WriteString(query)
			queryBuilder.WriteString(";\n")
			allArgs = append(allArgs, args...)
			// INSERT has rowsDelta of 1
			buildResults = append(buildResults, newDmlBuildResult(query, args, 1, nil))
		} else {
			// Normal event: use standard DML
			for _, buildResult := range this.buildDMLEventQuery(dmlEvent) {
				if buildResult.err != nil {
					return buildResult.err
				}
				queryBuilder.WriteString(buildResult.query)
				queryBuilder.WriteString(";\n")
				allArgs = append(allArgs, buildResult.args...)
				buildResults = append(buildResults, buildResult)
			}
		}
	}

	// Execute multi-statement
	execErr := conn.Raw(func(driverConn any) error {
		ex := driverConn.(driver.ExecerContext)
		nvc := driverConn.(driver.NamedValueChecker)

		namedArgs := make([]driver.NamedValue, 0, len(allArgs))
		for _, arg := range allArgs {
			nv := driver.NamedValue{Value: driver.Value(arg)}
			nvc.CheckNamedValue(&nv)
			namedArgs = append(namedArgs, nv)
		}

		res, err := ex.ExecContext(ctx, queryBuilder.String(), namedArgs)
		if err != nil {
			return fmt.Errorf("%w; query=%s", err, queryBuilder.String())
		}

		mysqlRes := res.(drivermysql.Result)
		for i, rowsAffected := range mysqlRes.AllRowsAffected() {
			*totalDelta += buildResults[i].rowsDelta * rowsAffected
		}
		return nil
	})

	if execErr != nil {
		return rollback(execErr)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	this.migrationContext.Log.Debugf("applyEventsOrderedMultiStatement: applied %d events, delta=%d", len(dmlEvents), *totalDelta)
	return nil
}

// buildInsertSelectFromOriginal builds INSERT...SELECT query for tracked PK
func (this *Applier) buildInsertSelectFromOriginal(dmlEvent *binlog.BinlogDMLEvent) (string, []interface{}) {
	var whereValues *sql.ColumnValues
	if dmlEvent.WhereColumnValues != nil {
		whereValues = dmlEvent.WhereColumnValues
	} else {
		whereValues = dmlEvent.NewColumnValues
	}

	query := fmt.Sprintf(`
		INSERT /* gh-ost */ INTO %s.%s (%s)
		SELECT %s FROM %s.%s WHERE %s
		ON DUPLICATE KEY UPDATE %s
	`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		strings.Join(this.migrationContext.SharedColumns.Names(), ", "),
		strings.Join(this.migrationContext.MappedSharedColumns.Names(), ", "),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		this.buildUniqueKeyWhereClause(),
		this.buildOnDuplicateUpdateClause(),
	)

	args := make([]interface{}, 0, this.migrationContext.UniqueKey.Len())
	for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
		ordinal := this.migrationContext.OriginalTableColumns.Ordinals[col.Name]
		args = append(args, whereValues.AbstractValues()[ordinal])
	}

	return query, args
}

// applyEventsWithFKFallback processes events one by one when batch fails with FK error
func (this *Applier) applyEventsWithFKFallback(ctx context.Context, dmlEvents []*binlog.BinlogDMLEvent, totalDelta *int64) error {
	for _, dmlEvent := range dmlEvents {
		var eventDelta int64
		pkKey := this.serializePKValues(dmlEvent)

		err := this.executeDMLEvents(ctx, []*binlog.BinlogDMLEvent{dmlEvent}, &eventDelta)
		if err != nil {
			if isForeignKeyError(err) {
				// FK error - apply via original table and track PK
				this.migrationContext.Log.Debugf("FK error for event, applying via original table")
				err = this.applyViaOriginalTable(ctx, dmlEvent, &eventDelta)
				if err != nil {
					return err
				}
				this.addFKTrackedPK(pkKey)
			} else {
				return err
			}
		}
		*totalDelta += eventDelta
	}
	return nil
}

// batchApplyViaOriginalTable applies multiple tracked events via original table in batch
func (this *Applier) batchApplyViaOriginalTable(ctx context.Context, dmlEvents []*binlog.BinlogDMLEvent) (int64, error) {
	if len(dmlEvents) == 0 {
		return 0, nil
	}

	var totalDelta int64
	conn, err := this.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	// Set session variables once for all queries
	sessionQuery := "SET /* gh-ost */ SESSION time_zone = '+00:00'"
	sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())
	if this.migrationContext.AcceptHighRiskOrphanedForeignKeys {
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, "foreign_key_checks = 0")
	}
	if _, err := conn.ExecContext(ctx, sessionQuery); err != nil {
		return 0, err
	}

	// Build batch query using UNION ALL for multiple rows
	whereClause := this.buildUniqueKeyWhereClause()
	sharedCols := strings.Join(this.migrationContext.SharedColumns.Names(), ", ")
	mappedCols := strings.Join(this.migrationContext.MappedSharedColumns.Names(), ", ")
	onDuplicateClause := this.buildOnDuplicateUpdateClause()

	// Build multi-row INSERT ... SELECT ... UNION ALL SELECT ...
	var queryBuilder strings.Builder
	var allArgs []interface{}

	queryBuilder.WriteString(fmt.Sprintf(`
		INSERT /* gh-ost */ INTO %s.%s (%s)
		SELECT * FROM (
	`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sharedCols,
	))

	for i, dmlEvent := range dmlEvents {
		if dmlEvent.DML == binlog.DeleteDML {
			// Handle DELETE separately
			var delDelta int64
			if err := this.executeDMLEvents(ctx, []*binlog.BinlogDMLEvent{dmlEvent}, &delDelta); err != nil {
				return totalDelta, err
			}
			totalDelta += delDelta
			continue
		}

		var whereValues *sql.ColumnValues
		if dmlEvent.WhereColumnValues != nil {
			whereValues = dmlEvent.WhereColumnValues
		} else {
			whereValues = dmlEvent.NewColumnValues
		}

		if i > 0 {
			queryBuilder.WriteString(" UNION ALL ")
		}
		queryBuilder.WriteString(fmt.Sprintf(`
			SELECT %s FROM %s.%s WHERE %s
		`,
			mappedCols,
			sql.EscapeName(this.migrationContext.DatabaseName),
			sql.EscapeName(this.migrationContext.OriginalTableName),
			whereClause,
		))

		// Add args for this event's WHERE clause
		for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
			ordinal := this.migrationContext.OriginalTableColumns.Ordinals[col.Name]
			allArgs = append(allArgs, whereValues.AbstractValues()[ordinal])
		}
	}

	if len(allArgs) == 0 {
		// All events were DELETEs, already processed
		return totalDelta, nil
	}

	queryBuilder.WriteString(fmt.Sprintf(`
		) AS src
		ON DUPLICATE KEY UPDATE %s
	`, onDuplicateClause))

	result, err := conn.ExecContext(ctx, queryBuilder.String(), allArgs...)
	if err != nil {
		return totalDelta, fmt.Errorf("batchApplyViaOriginalTable: %w; args_count=%d", err, len(allArgs))
	}

	rowsAffected, _ := result.RowsAffected()
	totalDelta += rowsAffected

	this.migrationContext.Log.Debugf("batchApplyViaOriginalTable: affected %d rows for %d events", rowsAffected, len(dmlEvents))
	return totalDelta, nil
}

// serializePKValues creates a string key from the unique key values of a DML event
func (this *Applier) serializePKValues(dmlEvent *binlog.BinlogDMLEvent) string {
	var values *sql.ColumnValues
	if dmlEvent.WhereColumnValues != nil {
		values = dmlEvent.WhereColumnValues
	} else if dmlEvent.NewColumnValues != nil {
		values = dmlEvent.NewColumnValues
	}
	if values == nil {
		return ""
	}

	// Extract unique key column values
	pkValues := make([]interface{}, 0, this.migrationContext.UniqueKey.Len())
	for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
		ordinal := this.migrationContext.OriginalTableColumns.Ordinals[col.Name]
		pkValues = append(pkValues, values.AbstractValues()[ordinal])
	}
	return fmt.Sprintf("%v", pkValues)
}

// isFKTrackedPK checks if a PK is in the tracked set
func (this *Applier) isFKTrackedPK(pkKey string) bool {
	this.fkTrackedPKsMux.RLock()
	defer this.fkTrackedPKsMux.RUnlock()
	return this.fkTrackedPKs[pkKey]
}

// addFKTrackedPK adds a PK to the tracked set
func (this *Applier) addFKTrackedPK(pkKey string) {
	this.fkTrackedPKsMux.Lock()
	defer this.fkTrackedPKsMux.Unlock()
	this.fkTrackedPKs[pkKey] = true
	this.migrationContext.Log.Debugf("FK tracked PK added: %s (total: %d)", pkKey, len(this.fkTrackedPKs))
}

// applyViaOriginalTable queries the original table and applies data via INSERT ON DUPLICATE KEY UPDATE
func (this *Applier) applyViaOriginalTable(ctx context.Context, dmlEvent *binlog.BinlogDMLEvent, eventDelta *int64) error {
	if dmlEvent.DML == binlog.DeleteDML {
		// For DELETE, just apply normally
		return this.executeDMLEvents(ctx, []*binlog.BinlogDMLEvent{dmlEvent}, eventDelta)
	}

	// Build WHERE clause from unique key values
	var whereValues *sql.ColumnValues
	if dmlEvent.WhereColumnValues != nil {
		whereValues = dmlEvent.WhereColumnValues
	} else {
		whereValues = dmlEvent.NewColumnValues
	}

	// Query original table for current data
	query := fmt.Sprintf(`
		INSERT /* gh-ost */ INTO %s.%s (%s)
		SELECT %s FROM %s.%s
		WHERE %s
		ON DUPLICATE KEY UPDATE %s
	`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		strings.Join(this.migrationContext.SharedColumns.Names(), ", "),
		strings.Join(this.migrationContext.MappedSharedColumns.Names(), ", "),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		this.buildUniqueKeyWhereClause(),
		this.buildOnDuplicateUpdateClause(),
	)

	// Build arguments from unique key values
	args := make([]interface{}, 0, this.migrationContext.UniqueKey.Len())
	for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
		ordinal := this.migrationContext.OriginalTableColumns.Ordinals[col.Name]
		args = append(args, whereValues.AbstractValues()[ordinal])
	}

	conn, err := this.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	sessionQuery := "SET /* gh-ost */ SESSION time_zone = '+00:00'"
	sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())
	if this.migrationContext.AcceptHighRiskOrphanedForeignKeys {
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, "foreign_key_checks = 0")
	}

	if _, err := conn.ExecContext(ctx, sessionQuery); err != nil {
		return err
	}

	result, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("applyViaOriginalTable: %w; query=%s; args=%v", err, query, args)
	}

	rowsAffected, _ := result.RowsAffected()
	if dmlEvent.DML == binlog.InsertDML {
		*eventDelta += rowsAffected
	}

	this.migrationContext.Log.Debugf("applyViaOriginalTable: affected %d rows", rowsAffected)
	return nil
}

// buildUniqueKeyWhereClause builds WHERE clause for unique key columns
func (this *Applier) buildUniqueKeyWhereClause() string {
	clauses := make([]string, 0, this.migrationContext.UniqueKey.Len())
	for _, col := range this.migrationContext.UniqueKey.Columns.Columns() {
		clauses = append(clauses, fmt.Sprintf("%s = ?", sql.EscapeName(col.Name)))
	}
	return strings.Join(clauses, " AND ")
}

// buildOnDuplicateUpdateClause builds ON DUPLICATE KEY UPDATE clause
func (this *Applier) buildOnDuplicateUpdateClause() string {
	clauses := make([]string, 0, this.migrationContext.SharedColumns.Len())
	for _, colName := range this.migrationContext.SharedColumns.Names() {
		clauses = append(clauses, fmt.Sprintf("%s = VALUES(%s)", sql.EscapeName(colName), sql.EscapeName(colName)))
	}
	return strings.Join(clauses, ", ")
}

// executeDMLEvents executes DML events in a single transaction
func (this *Applier) executeDMLEvents(ctx context.Context, dmlEvents []*binlog.BinlogDMLEvent, totalDelta *int64) error {
	conn, err := this.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	sessionQuery := "SET /* gh-ost */ SESSION time_zone = '+00:00'"
	sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, this.generateSqlModeQuery())

	if this.migrationContext.AcceptHighRiskOrphanedForeignKeys {
		sessionQuery = fmt.Sprintf("%s, %s", sessionQuery, "foreign_key_checks = 0")
	}

	if _, err := conn.ExecContext(ctx, sessionQuery); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}

	buildResults := make([]*dmlBuildResult, 0, len(dmlEvents))
	nArgs := 0
	for _, dmlEvent := range dmlEvents {
		for _, buildResult := range this.buildDMLEventQuery(dmlEvent) {
			if buildResult.err != nil {
				return rollback(buildResult.err)
			}
			nArgs += len(buildResult.args)
			buildResults = append(buildResults, buildResult)
		}
	}

	// We batch together the DML queries into multi-statements to minimize network trips.
	execErr := conn.Raw(func(driverConn any) error {
		ex := driverConn.(driver.ExecerContext)
		nvc := driverConn.(driver.NamedValueChecker)

		multiArgs := make([]driver.NamedValue, 0, nArgs)
		multiQueryBuilder := strings.Builder{}
		for _, buildResult := range buildResults {
			for _, arg := range buildResult.args {
				nv := driver.NamedValue{Value: driver.Value(arg)}
				nvc.CheckNamedValue(&nv)
				multiArgs = append(multiArgs, nv)
			}

			multiQueryBuilder.WriteString(buildResult.query)
			multiQueryBuilder.WriteString(";\n")
		}

		res, err := ex.ExecContext(ctx, multiQueryBuilder.String(), multiArgs)
		if err != nil {
			err = fmt.Errorf("%w; query=%s; args=%+v", err, multiQueryBuilder.String(), multiArgs)
			return err
		}

		mysqlRes := res.(drivermysql.Result)

		for i, rowsAffected := range mysqlRes.AllRowsAffected() {
			*totalDelta += buildResults[i].rowsDelta * rowsAffected
		}
		return nil
	})

	if execErr != nil {
		return rollback(execErr)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (this *Applier) Teardown() {
	this.migrationContext.Log.Debugf("Tearing down...")
	this.db.Close()
	this.singletonDB.Close()
	atomic.StoreInt64(&this.finishedMigrating, 1)
}

func (this *Applier) ExpectMetadataLock(sessionId int64) error {
	found := false
	query := `
		select /* gh-ost */ m.owner_thread_id
			from performance_schema.metadata_locks m join performance_schema.threads t 
			on m.owner_thread_id=t.thread_id
			where m.object_type = 'TABLE' and m.object_schema = ? and m.object_name = ? 
			and m.lock_type = 'EXCLUSIVE' and m.lock_status = 'PENDING' 
			and t.processlist_id = ?
	`
	err := sqlutils.QueryRowsMap(this.db, query, func(m sqlutils.RowMap) error {
		found = true
		return nil
	}, this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName, sessionId)
	if err != nil {
		return err
	}
	if !found {
		err = fmt.Errorf("cannot find PENDING metadata lock on original table: `%s`.`%s`", this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName)
		return this.migrationContext.Log.Errore(err)
	}
	return nil
}
