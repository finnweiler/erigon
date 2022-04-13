package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net"
	"strings"
	"time"
)

type DBSQLite struct {
	db *sql.DB
}

// language=SQL
const (
	sqlCreateSchema = `
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS nodes (
    id TEXT PRIMARY KEY,

    ip TEXT,
    port_disc INTEGER,
    port_rlpx INTEGER,
    ip_v6 TEXT,
    ip_v6_port_disc INTEGER,
    ip_v6_port_rlpx INTEGER,
    addr_updated INTEGER NOT NULL,

	ping_try INTEGER NOT NULL DEFAULT 0,

    compat_fork INTEGER,
    compat_fork_updated INTEGER,

    client_id TEXT,
    handshake_err TEXT,
    handshake_try INTEGER NOT NULL DEFAULT 0,
    handshake_updated INTEGER,
    
    neighbor_keys TEXT,
    
    taken_last INTEGER
);

CREATE INDEX IF NOT EXISTS idx_nodes_taken_last ON nodes (taken_last);
CREATE INDEX IF NOT EXISTS idx_nodes_ip ON nodes (ip);
CREATE INDEX IF NOT EXISTS idx_nodes_ip_v6 ON nodes (ip_v6);
CREATE INDEX IF NOT EXISTS idx_nodes_compat_fork ON nodes (compat_fork);
CREATE INDEX IF NOT EXISTS idx_nodes_handshake_updated ON nodes (handshake_updated);
`

	sqlUpsertNodeAddr = `
INSERT INTO nodes(
	id,
    ip,
    port_disc,
    port_rlpx,
    ip_v6,
    ip_v6_port_disc,
    ip_v6_port_rlpx,
    addr_updated
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    ip = excluded.ip,
    port_disc = excluded.port_disc,
    port_rlpx = excluded.port_rlpx,
    ip_v6 = excluded.ip_v6,
    ip_v6_port_disc = excluded.ip_v6_port_disc,
    ip_v6_port_rlpx = excluded.ip_v6_port_rlpx,
    addr_updated = excluded.addr_updated
`

	sqlFindNodeAddr = `
SELECT
    ip,
    port_disc,
    port_rlpx,
    ip_v6,
    ip_v6_port_disc,
    ip_v6_port_rlpx
FROM nodes
WHERE id = ?
`

	sqlResetPingError = `
UPDATE nodes SET ping_try = 0 WHERE id = ?
`

	sqlUpdatePingError = `
UPDATE nodes SET ping_try = nodes.ping_try + 1 WHERE id = ?
`

	sqlUpdateClientID = `
UPDATE nodes SET 
	client_id = ?, 
	handshake_err = NULL,
    handshake_try = 0,
	handshake_updated = ?
WHERE id = ?
`

	sqlUpdateHandshakeError = `
UPDATE nodes SET
	handshake_err = ?,
	handshake_try = nodes.handshake_try + 1,
	handshake_updated = ?
WHERE id = ?
`

	sqlFindHandshakeLastTry = `
SELECT
	handshake_err,
	handshake_try, 
	handshake_updated
FROM nodes
WHERE id = ?
`

	sqlFindHandshakeCandidates = `
SELECT id FROM nodes
WHERE ((handshake_updated IS NULL)
        OR ((handshake_updated < ?) AND (handshake_err IS NULL))
    	OR ((handshake_updated < ?) AND (handshake_err IS NOT NULL)))
	AND ((compat_fork == TRUE) OR (compat_fork IS NULL))
	AND ((handshake_try <= ?) OR (handshake_err = ?))
ORDER BY handshake_updated
LIMIT ?
`

	sqlMarkTakenHandshakeCandidates = `
UPDATE nodes SET handshake_updated = ?, handshake_err = 'taken' WHERE id IN (123)
`

	sqlUpdateForkCompatibility = `
UPDATE nodes SET compat_fork = ?, compat_fork_updated = ? WHERE id = ?
`

	sqlUpdateNeighborBucketKeys = `
UPDATE nodes SET neighbor_keys = ? WHERE id = ?
`

	sqlFindNeighborBucketKeys = `
SELECT neighbor_keys FROM nodes WHERE id = ?
`

	sqlFindCandidates = `
SELECT id FROM nodes
WHERE ((taken_last IS NULL) OR (taken_last < ?))
	AND ((compat_fork == TRUE) OR (compat_fork IS NULL))
    AND (ping_try <= ?)
	AND ((handshake_try <= ?) OR (handshake_err = ?))
ORDER BY taken_last
LIMIT ?
`

	sqlMarkTakenNodes = `
UPDATE nodes SET taken_last = ? WHERE id IN (123)
`

	sqlCountNodes = `
SELECT COUNT(id) FROM nodes
`

	sqlCountCompatibleNodes = `
SELECT COUNT(id) FROM nodes
WHERE (compat_fork == TRUE) OR (compat_fork IS NULL)
`

	sqlCountIPs = `
SELECT COUNT(DISTINCT ip) FROM nodes
`

	sqlEnumerateClientIDs = `
SELECT client_id FROM nodes
`
)

func NewDBSQLite(filePath string) (*DBSQLite, error) {
	db, err := sql.Open("sqlite", filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open DB: %w", err)
	}

	_, err = db.Exec(sqlCreateSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to create the DB schema: %w", err)
	}

	instance := DBSQLite{db}
	return &instance, nil
}

func (db *DBSQLite) UpsertNodeAddr(ctx context.Context, id NodeID, addr NodeAddr) error {
	var ip *string
	if addr.IP != nil {
		value := addr.IP.String()
		ip = &value
	}

	var ipV6 *string
	if addr.IPv6.IP != nil {
		value := addr.IPv6.IP.String()
		ipV6 = &value
	}

	var portDisc *int
	if (ip != nil) && (addr.PortDisc != 0) {
		value := int(addr.PortDisc)
		portDisc = &value
	}

	var ipV6PortDisc *int
	if (ipV6 != nil) && (addr.IPv6.PortDisc != 0) {
		value := int(addr.IPv6.PortDisc)
		ipV6PortDisc = &value
	}

	var portRLPx *int
	if (ip != nil) && (addr.PortRLPx != 0) {
		value := int(addr.PortRLPx)
		portRLPx = &value
	}

	var ipV6PortRLPx *int
	if (ipV6 != nil) && (addr.IPv6.PortRLPx != 0) {
		value := int(addr.IPv6.PortRLPx)
		ipV6PortRLPx = &value
	}

	updated := time.Now().Unix()

	_, err := db.db.ExecContext(ctx, sqlUpsertNodeAddr,
		id,
		ip, portDisc, portRLPx,
		ipV6, ipV6PortDisc, ipV6PortRLPx,
		updated)
	if err != nil {
		return fmt.Errorf("failed to upsert a node address: %w", err)
	}
	return nil
}

func (db *DBSQLite) FindNodeAddr(ctx context.Context, id NodeID) (*NodeAddr, error) {
	row := db.db.QueryRowContext(ctx, sqlFindNodeAddr, id)

	var ip sql.NullString
	var portDisc sql.NullInt32
	var portRLPx sql.NullInt32
	var ipV6 sql.NullString
	var ipV6PortDisc sql.NullInt32
	var ipV6PortRLPx sql.NullInt32

	err := row.Scan(
		&ip,
		&portDisc,
		&portRLPx,
		&ipV6,
		&ipV6PortDisc,
		&ipV6PortRLPx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("FindNodeAddr failed: %w", err)
	}

	var addr NodeAddr

	if ip.Valid {
		value := net.ParseIP(ip.String)
		if value == nil {
			return nil, errors.New("FindNodeAddr failed to parse IP")
		}
		addr.IP = value
	}
	if ipV6.Valid {
		value := net.ParseIP(ipV6.String)
		if value == nil {
			return nil, errors.New("FindNodeAddr failed to parse IPv6")
		}
		addr.IPv6.IP = value
	}
	if portDisc.Valid {
		value := uint16(portDisc.Int32)
		addr.PortDisc = value
	}
	if portRLPx.Valid {
		value := uint16(portRLPx.Int32)
		addr.PortRLPx = value
	}
	if ipV6PortDisc.Valid {
		value := uint16(ipV6PortDisc.Int32)
		addr.IPv6.PortDisc = value
	}
	if ipV6PortRLPx.Valid {
		value := uint16(ipV6PortRLPx.Int32)
		addr.IPv6.PortRLPx = value
	}

	return &addr, nil
}

func (db *DBSQLite) ResetPingError(ctx context.Context, id NodeID) error {
	_, err := db.db.ExecContext(ctx, sqlResetPingError, id)
	if err != nil {
		return fmt.Errorf("ResetPingError failed: %w", err)
	}
	return nil
}

func (db *DBSQLite) UpdatePingError(ctx context.Context, id NodeID) error {
	_, err := db.db.ExecContext(ctx, sqlUpdatePingError, id)
	if err != nil {
		return fmt.Errorf("UpdatePingError failed: %w", err)
	}
	return nil
}

func (db *DBSQLite) UpdateClientID(ctx context.Context, id NodeID, clientID string) error {
	updated := time.Now().Unix()

	_, err := db.db.ExecContext(ctx, sqlUpdateClientID, clientID, updated, id)
	if err != nil {
		return fmt.Errorf("UpdateClientID failed to update a node: %w", err)
	}
	return nil
}

func (db *DBSQLite) UpdateHandshakeError(ctx context.Context, id NodeID, handshakeErr string) error {
	updated := time.Now().Unix()

	_, err := db.db.ExecContext(ctx, sqlUpdateHandshakeError, handshakeErr, updated, id)
	if err != nil {
		return fmt.Errorf("UpdateHandshakeError failed to update a node: %w", err)
	}
	return nil
}

func (db *DBSQLite) FindHandshakeLastTry(ctx context.Context, id NodeID) (*HandshakeTry, error) {
	row := db.db.QueryRowContext(ctx, sqlFindHandshakeLastTry, id)

	var handshakeErr sql.NullString
	var tryNum sql.NullInt32
	var updatedTimestamp sql.NullInt64

	if err := row.Scan(&handshakeErr, &tryNum, &updatedTimestamp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("FindHandshakeLastTry failed: %w", err)
	}

	// if never we tried to handshake then the update time is NULL
	if !updatedTimestamp.Valid {
		return nil, nil
	}

	try := HandshakeTry{
		handshakeErr.Valid,
		uint(tryNum.Int32),
		time.Unix(updatedTimestamp.Int64, 0),
	}
	return &try, nil
}

func (db *DBSQLite) FindHandshakeCandidates(
	ctx context.Context,
	minUnusedOKDuration time.Duration,
	minUnusedErrDuration time.Duration,
	maxHandshakeTries uint,
	transientErr string,
	limit uint,
) ([]NodeID, error) {
	updatedOKBefore := time.Now().Add(-minUnusedOKDuration).Unix()
	updatedErrBefore := time.Now().Add(-minUnusedErrDuration).Unix()
	cursor, err := db.db.QueryContext(
		ctx,
		sqlFindHandshakeCandidates,
		updatedOKBefore,
		updatedErrBefore,
		maxHandshakeTries,
		transientErr,
		limit)
	if err != nil {
		return nil, fmt.Errorf("FindHandshakeCandidates failed to query candidates: %w", err)
	}
	defer func() {
		_ = cursor.Close()
	}()

	var nodes []NodeID
	for cursor.Next() {
		var id string
		err := cursor.Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("FindHandshakeCandidates failed to read candidate data: %w", err)
		}

		nodes = append(nodes, NodeID(id))
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("FindHandshakeCandidates failed to iterate over candidates: %w", err)
	}
	return nodes, nil
}

func (db *DBSQLite) MarkTakenHandshakeCandidates(ctx context.Context, ids []NodeID) error {
	if len(ids) == 0 {
		return nil
	}

	takenLast := time.Now().Unix()

	idsPlaceholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	query := strings.Replace(sqlMarkTakenHandshakeCandidates, "123", idsPlaceholders, 1)
	args := append([]interface{}{takenLast}, stringsToAny(ids)...)

	_, err := db.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to mark taken handshake candidates: %w", err)
	}
	return nil
}

func (db *DBSQLite) TakeHandshakeCandidates(
	ctx context.Context,
	minUnusedOKDuration time.Duration,
	minUnusedErrDuration time.Duration,
	maxHandshakeTries uint,
	transientErr string,
	limit uint,
) ([]NodeID, error) {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("TakeHandshakeCandidates failed to start transaction: %w", err)
	}

	ids, err := db.FindHandshakeCandidates(
		ctx,
		minUnusedOKDuration,
		minUnusedErrDuration,
		maxHandshakeTries,
		transientErr,
		limit)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	err = db.MarkTakenHandshakeCandidates(ctx, ids)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("TakeHandshakeCandidates failed to commit transaction: %w", err)
	}
	return ids, nil
}

func (db *DBSQLite) UpdateForkCompatibility(ctx context.Context, id NodeID, isCompatFork bool) error {
	updated := time.Now().Unix()

	_, err := db.db.ExecContext(ctx, sqlUpdateForkCompatibility, isCompatFork, updated, id)
	if err != nil {
		return fmt.Errorf("UpdateForkCompatibility failed to update a node: %w", err)
	}
	return nil
}

func (db *DBSQLite) UpdateNeighborBucketKeys(ctx context.Context, id NodeID, keys []string) error {
	keysStr := strings.Join(keys, ",")

	_, err := db.db.ExecContext(ctx, sqlUpdateNeighborBucketKeys, keysStr, id)
	if err != nil {
		return fmt.Errorf("UpdateNeighborBucketKeys failed to update a node: %w", err)
	}
	return nil
}

func (db *DBSQLite) FindNeighborBucketKeys(ctx context.Context, id NodeID) ([]string, error) {
	row := db.db.QueryRowContext(ctx, sqlFindNeighborBucketKeys, id)

	var keysStr sql.NullString
	if err := row.Scan(&keysStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("FindNeighborBucketKeys failed: %w", err)
	}

	if !keysStr.Valid {
		return nil, nil
	}
	return strings.Split(keysStr.String, ","), nil
}

func (db *DBSQLite) FindCandidates(
	ctx context.Context,
	minUnusedDuration time.Duration,
	maxPingTries uint,
	maxHandshakeTries uint,
	transientHandshakeErr string,
	limit uint,
) ([]NodeID, error) {
	takenLastBefore := time.Now().Add(-minUnusedDuration).Unix()
	cursor, err := db.db.QueryContext(
		ctx,
		sqlFindCandidates,
		takenLastBefore,
		maxPingTries,
		maxHandshakeTries,
		transientHandshakeErr,
		limit)
	if err != nil {
		return nil, fmt.Errorf("FindCandidates failed to query candidates: %w", err)
	}
	defer func() {
		_ = cursor.Close()
	}()

	var nodes []NodeID
	for cursor.Next() {
		var id string
		err := cursor.Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("FindCandidates failed to read candidate data: %w", err)
		}

		nodes = append(nodes, NodeID(id))
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("FindCandidates failed to iterate over candidates: %w", err)
	}
	return nodes, nil
}

func (db *DBSQLite) MarkTakenNodes(ctx context.Context, ids []NodeID) error {
	if len(ids) == 0 {
		return nil
	}

	takenLast := time.Now().Unix()

	idsPlaceholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	query := strings.Replace(sqlMarkTakenNodes, "123", idsPlaceholders, 1)
	args := append([]interface{}{takenLast}, stringsToAny(ids)...)

	_, err := db.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to mark taken nodes: %w", err)
	}
	return nil
}

func (db *DBSQLite) TakeCandidates(
	ctx context.Context,
	minUnusedDuration time.Duration,
	maxPingTries uint,
	maxHandshakeTries uint,
	transientHandshakeErr string,
	limit uint,
) ([]NodeID, error) {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("TakeCandidates failed to start transaction: %w", err)
	}

	ids, err := db.FindCandidates(
		ctx,
		minUnusedDuration,
		maxPingTries,
		maxHandshakeTries,
		transientHandshakeErr,
		limit)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	err = db.MarkTakenNodes(ctx, ids)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("TakeCandidates failed to commit transaction: %w", err)
	}
	return ids, nil
}

func (db *DBSQLite) IsConflictError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "SQLITE_BUSY")
}

func (db *DBSQLite) CountNodes(ctx context.Context) (uint, error) {
	row := db.db.QueryRowContext(ctx, sqlCountNodes)
	var count uint
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("CountNodes failed: %w", err)
	}
	return count, nil
}

func (db *DBSQLite) CountCompatibleNodes(ctx context.Context) (uint, error) {
	row := db.db.QueryRowContext(ctx, sqlCountCompatibleNodes)
	var count uint
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("CountCompatibleNodes failed: %w", err)
	}
	return count, nil
}

func (db *DBSQLite) CountIPs(ctx context.Context) (uint, error) {
	row := db.db.QueryRowContext(ctx, sqlCountIPs)
	var count uint
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("CountIPs failed: %w", err)
	}
	return count, nil
}

func (db *DBSQLite) EnumerateClientIDs(ctx context.Context, enumFunc func(clientID *string)) error {
	cursor, err := db.db.QueryContext(ctx, sqlEnumerateClientIDs)
	if err != nil {
		return fmt.Errorf("EnumerateClientIDs failed to query: %w", err)
	}
	defer func() {
		_ = cursor.Close()
	}()

	for cursor.Next() {
		var clientID sql.NullString
		err := cursor.Scan(&clientID)
		if err != nil {
			return fmt.Errorf("EnumerateClientIDs failed to read data: %w", err)
		}
		if clientID.Valid {
			enumFunc(&clientID.String)
		} else {
			enumFunc(nil)
		}
	}

	if err := cursor.Err(); err != nil {
		return fmt.Errorf("EnumerateClientIDs failed to iterate: %w", err)
	}
	return nil
}

func stringsToAny(strValues []NodeID) []interface{} {
	values := make([]interface{}, 0, len(strValues))
	for _, value := range strValues {
		values = append(values, value)
	}
	return values
}
