package enginedb

import (
	"context"
	"database/sql"
	"fmt"
)

// normalizePlaylistTails joins disconnected NULL-terminated native chains before
// converting NULL to zero. Converting several sibling tails directly to zero
// violates Engine's UNIQUE(parentListId,nextListId) constraint.
func normalizePlaylistTails(ctx context.Context, tx *sql.Tx) error {
	type node struct {
		id   int64
		next sql.NullInt64
	}
	groups := map[sql.NullInt64][]node{}
	rows, err := tx.QueryContext(ctx, `SELECT id,parentListId,nextListId FROM Playlist ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n node
		var parent sql.NullInt64
		if err = rows.Scan(&n.id, &parent, &n.next); err != nil {
			rows.Close()
			return err
		}
		groups[parent] = append(groups[parent], n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, nodes := range groups {
		previous := map[int64]int64{}
		var nulls []int64
		var tail int64
		hasTail := false
		for _, n := range nodes {
			if !n.next.Valid {
				nulls = append(nulls, n.id)
			}
		}
		if len(nulls) == 0 {
			continue
		}
		for _, n := range nodes {
			if !n.next.Valid {
				continue
			}
			if n.next.Int64 == 0 {
				tail = n.id
				hasTail = true
				continue
			}
			if _, exists := previous[n.next.Int64]; exists {
				return fmt.Errorf("ambiguous native playlist chain")
			}
			previous[n.next.Int64] = n.id
		}
		for _, id := range nulls {
			head := id
			for hops := 0; ; hops++ {
				predecessor, exists := previous[head]
				if !exists {
					break
				}
				if hops >= len(nodes) {
					return fmt.Errorf("cyclic native playlist chain")
				}
				head = predecessor
			}
			if hasTail {
				if _, err = tx.ExecContext(ctx, `UPDATE Playlist SET nextListId=? WHERE id=?`, head, tail); err != nil {
					return err
				}
			}
			tail = id
			hasTail = true
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE Playlist SET nextListId=0 WHERE nextListId IS NULL`)
	return err
}
