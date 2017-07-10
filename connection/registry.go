package connection

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/dqlite/go-sqlite3x"
	"github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
)

// Registry is a DQLite node-level data structure that tracks all
// SQLite connections opened on the node, either in leader replication
// mode or follower replication mode.
type Registry struct {
	mu             sync.RWMutex                   // Serialize access to internal state
	dir            string                         // Directory where we store database files
	names          map[string]*DSN                // Map database identifiers to their DSN
	leaders        map[*sqlite3.SQLiteConn]string // Leader connections to database names
	followers      map[string]*sqlite3.SQLiteConn // Database names to follower connections
	autoCheckpoint int                            // Number for WAL frames after which a checkpoint will be triggered
}

// NewRegistry creates a new connections registry, managing
// connections against database files in the given directory.
func NewRegistry(dir string) *Registry {
	return &Registry{
		dir:            dir,
		names:          map[string]*DSN{},
		leaders:        map[*sqlite3.SQLiteConn]string{},
		followers:      map[string]*sqlite3.SQLiteConn{},
		autoCheckpoint: 1000, // Same as SQLite default value
	}
}

// Dir is the directory where databases are kept.
func (r *Registry) Dir() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.dir
}

// AutoCheckpoint sets the auto-checkpoint threshold.
func (r *Registry) AutoCheckpoint(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.autoCheckpoint = n
}

// Add a new database that will be replicated by DQLite. Within this
// registry, the database must be uniquely identified by the given
// name, and the given dsn will be used when creating connections to
// it.
func (r *Registry) Add(name string, dsn *DSN) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.names[name]; ok {
		panic(fmt.Sprintf("name '%s' is already registered", name))
	}

	r.names[name] = dsn
}

// DSN returns the DSN associated with the database with the given name.
func (r *Registry) DSN(database string) *DSN {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.dsn(database)
}

// NameByLeader returns the name of the database associated with the given
// connection, which is assumed to be a registered leader connection.
func (r *Registry) NameByLeader(conn *sqlite3.SQLiteConn) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name, ok := r.leaders[conn]
	if !ok {
		panic("no database for the given connection")
	}
	return name
}

// AllNames returns the names for all databases currently registered.
func (r *Registry) AllNames() []string {
	names := []string{}
	for name := range r.names {
		names = append(names, name)
	}
	return names
}

// OpenFollower opens a follower against the database identified by
// the given name.
func (r *Registry) OpenFollower(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.followers[name]; ok {
		panic(fmt.Sprintf("follower connection for '%s' already open", name))
	}

	errWrapper := func(err error) error {
		return errors.Wrap(err, "failed to open follower connection")
	}

	dsn := r.dsn(name)
	conn, err := r.open(dsn)
	if err != nil {
		return errWrapper(err)
	}

	// Ensure WAL autocheckpoint for followers is disabled
	if err := sqlite3x.WalAutoCheckpointPragma(conn, 0); err != nil {
		return err
	}

	// Swith to leader replication mode for this connection.
	if err := sqlite3x.ReplicationFollower(conn); err != nil {
		return errWrapper(err)
	}

	r.followers[name] = conn

	return nil

}

// CloseFollower closes a follower connection.
func (r *Registry) CloseFollower(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	conn := r.follower(name)
	delete(r.followers, name)
	return conn.Close()
}

// Follower returns the follower connection used to replicate the
// database identified by the given name.
func (r *Registry) Follower(name string) *sqlite3.SQLiteConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.follower(name)
}

// OpenLeader returns a new SQLite connection to a database in our
// directory, set in leader replication mode.
func (r *Registry) OpenLeader(name string, methods sqlite3x.ReplicationMethods) (*sqlite3.SQLiteConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dsn := r.dsn(name)

	errWrapper := func(err error) error {
		return errors.Wrap(err, "failed to open leader connection")
	}

	conn, err := r.open(dsn)
	if err != nil {
		return nil, errWrapper(err)
	}

	// Ensure WAL autocheckpoint is set
	sqlite3x.ReplicationAutoCheckpoint(conn, r.autoCheckpoint)

	// Swith to leader replication mode for this connection.
	if err := sqlite3x.ReplicationLeader(conn, methods); err != nil {
		return nil, errWrapper(err)
	}

	r.leaders[conn] = name

	return conn, nil

}

// CloseLeader closes the given leader connection and deletes its entry
// in the leaders map.
func (r *Registry) CloseLeader(conn *sqlite3.SQLiteConn) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.leaders[conn]; !ok {
		panic("attempt to close a connection that was not registered")
	}

	// XXX Also set replication to none, to clear methods memory. Perhaps
	//     this should be done in sqlite3x in a more explicit or nicer way.
	if _, err := sqlite3x.ReplicationNone(conn); err != nil {
		return err
	}

	if err := conn.Close(); err != nil {
		return err
	}

	delete(r.leaders, conn)

	return nil
}

// Leaders returns all open leader connections for the database with
// the given name.
func (r *Registry) Leaders(name string) []*sqlite3.SQLiteConn {
	r.mu.RLock()
	defer r.mu.RUnlock()

	conns := []*sqlite3.SQLiteConn{}
	for conn := range r.leaders {
		if r.leaders[conn] == name {
			conns = append(conns, conn)
		}
	}
	return conns
}

// Backup a single database using the given leader connection. It
// returns two slices of data, one the content of the backup database
// and one is the current content of the WAL file.
func (r *Registry) Backup(name string) ([]byte, []byte, error) {
	//name := r.NameByLeader(conn)
	sourceDSN := r.DSN(name)
	sourceConn, err := r.open(sourceDSN)
	if err != nil {
		return nil, nil, err
	}
	defer sourceConn.Close()

	backupConn, err := r.openBackup(sourceDSN)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range []string{
		sqlite3x.DatabaseFilename(backupConn),
		sqlite3x.WalFilename(backupConn),
		sqlite3x.ShmFilename(backupConn),
	} {
		defer os.Remove(path)
	}
	defer backupConn.Close()

	backup, err := backupConn.Backup("main", sourceConn, "main")
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to init backup database")
	}

	done, err := backup.Step(-1)
	backup.Close()
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to backup database")
	}
	if !done {
		return nil, nil, fmt.Errorf("database backup not complete")
	}

	database, err := r.readDatabaseContent(sourceConn)
	if err != nil {
		return nil, nil, err
	}

	wal, err := r.readWalContent(backupConn)
	if err != nil {
		return nil, nil, err
	}

	return database, wal, nil
}

// Restore the given database and WAL backups.
func (r *Registry) Restore(name string, database []byte, wal []byte) error {
	if err := r.writeDatabaseContent(name, database); err != nil {
		return err
	}
	if err := r.writeWalContent(name, wal); err != nil {
		return err
	}
	return nil
}

// Purge removes all database files in our directory, including the
// directory itself.
func (r *Registry) Purge() error {
	for conn := range r.leaders {
		r.CloseLeader(conn)
	}
	for name := range r.followers {
		r.CloseFollower(name)
	}
	return os.RemoveAll(r.dir)
}

// Open returns a new SQLite connection to a database in our
// directory, configured with WAL journaling (automatic checkpoints
// are disabled and the WAL always kept persistent after connections
// close). The given DSN will be tracked in the registry and
// associated with the connection.
func (r *Registry) open(dsn *DSN) (*sqlite3.SQLiteConn, error) {
	driver := &sqlite3.SQLiteDriver{}
	conn, err := driver.Open(dsn.String(r.dir))
	if err != nil {
		return nil, err
	}
	// Convert driver.Conn interface to concrete sqlite3.SQLiteConn.
	sqliteConn := conn.(*sqlite3.SQLiteConn)

	// Ensure journal mode is set to WAL
	if err := sqlite3x.JournalModePragma(sqliteConn, sqlite3x.JournalWal); err != nil {
		return nil, err
	}

	// Ensure we don't truncate the WAL on exit.
	if err := sqlite3x.JournalSizeLimitPragma(sqliteConn, -1); err != nil {
		return nil, err
	}

	if err := sqlite3x.DatabaseNoCheckpointOnClose(sqliteConn); err != nil {
		return nil, err
	}

	return sqliteConn, nil
}

// Open a new database connection against a temporary backup database
// file named against the given DSN.
func (r *Registry) openBackup(dsn *DSN) (*sqlite3.SQLiteConn, error) {
	// Create a temporary file using the source DSN filename as prefix.
	tempFile, err := ioutil.TempFile(r.dir, dsn.Filename)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp file for backup")
	}
	tempFile.Close()

	backupDSN := &DSN{
		Filename: path.Base(tempFile.Name()),
		Query:    dsn.Query,
	}
	backupConn, err := r.open(backupDSN)
	if err != nil {
		os.Remove(tempFile.Name())
		return nil, errors.Wrap(err, "failed to open backup database")
	}
	return backupConn, nil
}

// Read the current content of the database file associated with the given
// connection.
func (r *Registry) readDatabaseContent(conn *sqlite3.SQLiteConn) ([]byte, error) {
	path := sqlite3x.DatabaseFilename(conn)
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("failed to read database content at %s", path))
	}
	return data, nil
}

// Read the current content of the WAL associated with the given
// connection.
func (r *Registry) readWalContent(conn *sqlite3.SQLiteConn) ([]byte, error) {
	path := sqlite3x.WalFilename(conn)
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("failed to read WAL content at %s", path))
	}
	return data, nil
}

// Write the the content of a database backup to the DSN filename associated
// with the given identifier.
func (r *Registry) writeDatabaseContent(name string, database []byte) error {
	path := filepath.Join(r.Dir(), r.DSN(name).Filename)
	if err := ioutil.WriteFile(path, database, 0600); err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to write database content at %s", path))
	}
	return nil
}

// Write the the content of a WAL backup to the DSN filename associated
// with the given identifier.
func (r *Registry) writeWalContent(name string, wal []byte) error {
	path := filepath.Join(r.Dir(), r.DSN(name).Filename+"-wal")
	if err := ioutil.WriteFile(path, wal, 0600); err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to write wal content at %s", path))
	}
	return nil
}

// Return the follower connection associated with the database with
// the given name, panics if not there.
func (r *Registry) follower(name string) *sqlite3.SQLiteConn {
	conn, ok := r.followers[name]
	if !ok {
		panic(fmt.Sprintf("no follower connection for '%s'", name))
	}
	return conn
}

// Return the DSN associated with the database with the given name,
// panics if not there.
func (r *Registry) dsn(name string) *DSN {
	dsn, ok := r.names[name]
	if !ok {
		panic(fmt.Sprintf("dsn not found: '%s' is not registered", name))
	}
	return dsn
}
